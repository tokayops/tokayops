package engine

import (
	"context"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

type Engine struct {
	store  store.StoreInterface
	oncall builders.OnCallProjection
	cfg    *config.Config
}

// NewEngine builds the alert engine. The on-call projection is shared with the
// escalation builder rather than constructed here: the snapshot the engine
// stores on an alert group and the users the builder escalates to must be the
// same answer, and two projections would be two clocks.
func NewEngine(s store.StoreInterface, oncall builders.OnCallProjection, cfg *config.Config) *Engine {
	return &Engine{
		store:  s,
		oncall: oncall,
		cfg:    cfg,
	}
}

func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.ProcessNewAlertGroups(ctx)
		}
	}
}

func (e *Engine) ProcessNewAlertGroups(ctx context.Context) {
	metrics.EngineRunsTotal.Inc()
	alertGroups, err := e.store.GetNewAlertGroups()
	if err != nil {
		log.Printf("AlertEngine: Error fetching new alert groups: %v", err)
		return
	}
	metrics.EngineAlertGroupsPickedTotal.Add(float64(len(alertGroups)))

	for _, ag := range alertGroups {
		if ag.Status == model.AlertGroupStatusProcessing {
			log.Printf("AlertEngine: Reconciling stale processing AG %s (stuck since %s)", ag.ID, ag.UpdatedAt.Format(time.RFC3339))
		}
		log.Printf("AlertEngine: Processing %s (Team: %s, Sev: %s)", ag.ID, ag.TeamID, ag.Severity)

		// Resolve Policy (may be empty - that's OK for firehose-only)
		policyID := e.resolvePolicy(ag.TeamID, ag.Severity)

		// Build unified job (includes firehose as step 0 if configured)
		escBuilder := builders.NewEscalationJobBuilder(e.store, e.oncall, e.cfg)
		job, stages, steps, snapshot, err := escBuilder.Build(ag, policyID)

		if err != nil {
			// Build failed — leave AG as "new" so it retries on next tick
			log.Printf("AlertEngine: Failed to build job for %s (will retry): %v", ag.ID, err)
			continue
		}

		if job != nil {
			// Atomic: lock AG row, check status, transition to processing,
			// create job with dedup, save snapshot — all in one TX.
			created, err := e.store.EnsureEscalationJob(ag.ID, job, stages, steps, snapshot)
			if err != nil {
				log.Printf("AlertEngine: Failed to ensure job for %s (will retry): %v", ag.ID, err)
				continue
			}
			if created {
				log.Printf("AlertEngine: Job created for %s (policy=%s, steps=%d)", ag.ID, snapshot.PolicyID, len(steps))
			} else {
				log.Printf("AlertEngine: Job already exists or AG status changed for %s", ag.ID)
			}
		} else {
			// job == nil: no firehose and no policy
			// CAS new→processing (for new AG) + touch updated_at (for stale processing)
			if ag.Status == model.AlertGroupStatusNew {
				if _, err := e.store.TransitionAlertGroupStatus(ag.ID, model.AlertGroupStatusNew, model.AlertGroupStatusProcessing); err != nil {
					log.Printf("AlertEngine: Failed to transition %s to processing: %v", ag.ID, err)
				}
			} else {
				// Stale processing without job — touch updated_at to prevent re-pickup
				if _, err := e.store.TransitionAlertGroupStatus(ag.ID, model.AlertGroupStatusProcessing, model.AlertGroupStatusProcessing); err != nil {
					log.Printf("AlertEngine: Failed to touch %s: %v", ag.ID, err)
				}
			}
			log.Printf("AlertEngine: No job created for %s (no firehose, no policy)", ag.ID)
		}

		// Save OnCall snapshot (regardless of job creation)
		oncallSnapshot, err := e.onCallSnapshot(ctx, ag.TeamID)
		if err != nil {
			log.Printf("AlertEngine: Failed to fetch oncall for %s: %v", ag.ID, err)
		} else if err := e.store.UpdateAlertGroupOnCall(ag.ID, oncallSnapshot); err != nil {
			log.Printf("AlertEngine: Failed to save oncall snapshot for %s: %v", ag.ID, err)
		}
	}
}

// onCallSnapshot records who was on duty when the alert arrived.
//
// A team with no schedule, or one between shifts, gets an empty snapshot rather
// than none: "nobody was on call" is a fact worth having on the alert group, and
// the readers of the field already treat an empty group as exactly that.
//
// Source is what survives of the override information now that the projection
// answers instead of a legacy override row: L1Users already names the stand-in,
// and Source says that is why.
func (e *Engine) onCallSnapshot(ctx context.Context, teamID string) (*model.OnCallResult, error) {
	team, err := e.oncall.CurrentTeamOnCallNow(ctx, teamID)
	if err != nil {
		return nil, err
	}

	out := &model.OnCallResult{}
	if l1 := team.OnCall.L1; l1 != nil {
		users, err := e.usersByIDs(l1.UserIDs)
		if err != nil {
			return nil, err
		}
		since, until := l1.AssignmentStart, l1.AssignmentEnd
		out.L1Users = users
		out.L1Since = &since
		out.L1Until = &until
		out.Source = l1.Source
	}
	if l2 := team.OnCall.L2; l2 != nil && len(l2.UserIDs) > 0 {
		users, err := e.usersByIDs(l2.UserIDs[:1])
		if err != nil {
			return nil, err
		}
		if len(users) > 0 {
			out.L2User = users[0]
		}
	}
	return out, nil
}

// usersByIDs hydrates IDs into user records, preserving the projection's order
// and dropping anyone the store no longer has.
func (e *Engine) usersByIDs(ids []string) ([]*model.User, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	fetched, err := e.store.GetUsersByIDs(ids)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*model.User, len(fetched))
	for _, u := range fetched {
		byID[u.ID] = u
	}
	out := make([]*model.User, 0, len(ids))
	for _, id := range ids {
		if u, ok := byID[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

func (e *Engine) resolvePolicy(teamID, severity string) string {
	// Try to get team from Store
	team, err := e.store.GetTeamByID(teamID)
	if err != nil {
		log.Printf("AlertEngine: Team '%s' not found: %v", teamID, err)
		return ""
	}

	// Check severity-specific route
	if team.SeverityRoutes != nil {
		if policyID, ok := team.SeverityRoutes[severity]; ok && policyID != "" {
			return policyID
		}
	}

	// Fall back to default policy
	return team.DefaultPolicyID
}
