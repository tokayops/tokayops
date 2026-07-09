package engine

import (
	"context"
	"log"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduler"
	"github.com/tokayops/tokayops/internal/store"
)

type Engine struct {
	store store.StoreInterface
	cfg   *config.Config
}

func NewEngine(s store.StoreInterface, cfg *config.Config) *Engine {
	return &Engine{
		store: s,
		cfg:   cfg,
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
			e.ProcessNewAlertGroups()
		}
	}
}

func (e *Engine) ProcessNewAlertGroups() {
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
		escBuilder := builders.NewEscalationJobBuilder(e.store, e.cfg)
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
		schedule, err := e.store.GetScheduleByTeamID(ag.TeamID)
		if err == nil && schedule != nil {
			oncallSnapshot, err := scheduler.FetchCurrentOnCall(e.store, schedule.ID)
			if err != nil {
				log.Printf("AlertEngine: Failed to fetch oncall for %s: %v", ag.ID, err)
			} else if oncallSnapshot != nil {
				if err := e.store.UpdateAlertGroupOnCall(ag.ID, oncallSnapshot); err != nil {
					log.Printf("AlertEngine: Failed to save oncall snapshot for %s: %v", ag.ID, err)
				}
			}
		}
	}
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
