package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/dispatcher/builders"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// onCallProjection is what the engine needs from the schedule projection.
//
// It is declared here rather than borrowed from the escalation builder because
// this is the engine's dependency: taking the builder's interface would make
// the engine's contract change whenever the builder's needs did, for no reason
// beyond the two happening to want the same thing today.
//
// Both methods are named although the engine calls one. The other is what it
// hands to the builder it constructs, and an interface that omitted it would
// describe less than the engine actually requires of the value it is given.
type onCallProjection interface {
	CurrentTeamOnCallNow(ctx context.Context, teamID string) (schedulerender.TeamOnCall, error)
	CurrentOnCallNow(ctx context.Context, scheduleID string) (schedulerender.OnCall, error)
}

type Engine struct {
	store      escalationStore
	oncall     onCallProjection
	escBuilder *builders.EscalationJobBuilder
}

// NewEngine builds the alert engine. The on-call projection is shared with the
// escalation builder rather than constructed here: the snapshot the engine
// stores on an alert group and the users the builder escalates to must be the
// same answer, and two projections would be two clocks.
//
// The builder is built once, here, rather than per alert group. It holds no
// state between builds - what one build remembers lives for that build - so a
// shared instance is the same object the loop was allocating each time round.
// escalationStore is the store as the escalation engine needs it: find the
// groups nobody has been paged for, learn who to page, and admit the escalation.
//
// EnsureEscalationJob is the one that carries the boundary. It is not "insert a
// job": in one commit it moves the group to processing, stores the policy
// snapshot the escalation will be executed against, and admits the job under its
// forever claim. Split into separate calls, a crash between them leaves a group
// that looks handled and is not.
type escalationStore interface {
	GetNewAlertGroups() ([]*model.AlertGroup, error)
	TransitionAlertGroupStatus(id string, fromStatus, toStatus model.AlertGroupStatus) (bool, error)
	UpdateAlertGroupOnCall(id string, snapshot *model.OnCallResult) error
	GetTeamByID(id string) (*model.Team, error)
	GetUsersByIDs(ids []string) ([]*model.User, error)

	// Passed down to the escalation builder, which needs the policy this group
	// is escalated by. Listed here because the engine hands its own store to
	// it: whatever the builder may read, the engine may read.
	GetEscalationPolicyByID(id string) (*model.EscalationPolicy, error)

	EnsureEscalationJob(agID string, job *model.Job, stages []*model.JobStage,
		steps []*model.JobStep, snapshot *model.EscalationPolicySnapshot) (bool, error)
}

func NewEngine(s escalationStore, oncall onCallProjection, cfg *config.Config) *Engine {
	// cfg is not kept: the only thing the engine did with it was hand it to the
	// builder, which now holds it.
	return &Engine{
		store:      s,
		oncall:     oncall,
		escBuilder: builders.NewEscalationJobBuilder(s, oncall, cfg),
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

	// The tick names the batch before it touches it, because every other line
	// below is written after the work it describes. Without this one, a hang or
	// a panic - in resolvePolicy, which reads the database without a context at
	// all, or anywhere inside Build - leaves nothing at all to go on, and a
	// poison group in a crash loop cannot be narrowed down.
	//
	// One line per tick rather than one per group: a group that cannot be built
	// is back in the next tick's batch, so anything written per group is written
	// again every second for as long as the trouble lasts.
	//
	// It BOUNDS the search, it does not name the culprit, and the two ways it
	// falls short are worth knowing before relying on it. A group deferred
	// earlier in the same tick has no Processing line either, and the tally that
	// would have named it never runs when a later group hangs - so the candidates
	// are every listed group without a Processing line, not one. And past the cap
	// the ids are not printed at all. Naming the group exactly needs a watchdog
	// over the group in flight, which is separate work.
	if len(alertGroups) > 0 {
		log.Printf("AlertEngine: tick picked %d group(s): %s", len(alertGroups), alertGroupIDs(alertGroups))
	}

	// A deferred alert group stays "new", so this tick's answer is next tick's
	// question. The count, the first group and the first error are collected
	// here and reported once for the whole tick instead.
	deferred := 0
	deferredFirst := ""
	var deferredErr error

	for _, ag := range alertGroups {
		// Resolve Policy (may be empty - that's OK for firehose-only)
		policyID := e.resolvePolicy(ag.TeamID, ag.Severity)

		// Who is on duty is read ONCE per alert group, and the same answer both
		// escalates and is recorded. Reading it again for the snapshot would put
		// a handoff or a save between the two, and the alert group would then
		// claim one group was on call while the job paged another.
		//
		// The outcome travels with the answer. A read that failed must not reach
		// the builder as "this team has no schedule": that is the one state that
		// sends it looking for the schedule ID stored on the policy step, which
		// may name a schedule this team no longer owns.
		teamOnCall := schedulerender.TeamOnCallRead(e.oncall.CurrentTeamOnCallNow(ctx, ag.TeamID))

		// Build unified job (includes firehose as step 0 if configured)
		job, stages, steps, snapshot, err := e.escBuilder.Build(ctx, ag, policyID, teamOnCall)

		// The recipients could not be resolved, so no job is committed and the
		// alert group stays "new" for the next tick to try again. Committing
		// one would spend this alert's only chance to page its on-call: nothing
		// picks up an alert group that already has an escalation job.
		//
		// Nothing is logged for this group: it will be back next tick, and the
		// tick after that. The tally declared above the loop reports all of
		// them in one line once the loop is done.
		if errors.Is(err, builders.ErrOnCallResolutionUnavailable) {
			metrics.EngineEscalationBuildDeferralsTotal.Inc()
			deferred++
			if deferredErr == nil {
				deferredErr = err
				deferredFirst = ag.ID
			}
			continue
		}

		// Everything the tick says about this group individually is said here,
		// after the decision to keep it, and never for a group it deferred.
		if ag.Status == model.AlertGroupStatusProcessing {
			log.Printf("AlertEngine: Reconciling stale processing AG %s (stuck since %s)", ag.ID, ag.UpdatedAt.Format(time.RFC3339))
		}
		log.Printf("AlertEngine: Processing %s (Team: %s, Sev: %s)", ag.ID, ag.TeamID, ag.Severity)
		// Still reachable, and worth one line: a policy with no schedule-typed
		// step builds fine without this answer, and damaged schedule data
		// degrades to a marker rather than deferring. Both commit a job, so
		// this group is processed once rather than every tick.
		if err := teamOnCall.Err(); err != nil {
			log.Printf("AlertEngine: Failed to fetch oncall for %s: %v", ag.ID, err)
		}

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

		// Save OnCall snapshot (regardless of job creation), from the very
		// projection the job was built from. A tick that could not read it
		// writes nothing rather than recording "nobody was on call", which is a
		// claim about the schedule and not about the database.
		if teamOnCall.Err() != nil {
			continue
		}
		oncallSnapshot, err := e.onCallSnapshot(teamOnCall.OnCall())
		if err != nil {
			log.Printf("AlertEngine: Failed to resolve oncall users for %s: %v", ag.ID, err)
		} else if err := e.store.UpdateAlertGroupOnCall(ag.ID, oncallSnapshot); err != nil {
			log.Printf("AlertEngine: Failed to save oncall snapshot for %s: %v", ag.ID, err)
		}
	}

	if deferred > 0 {
		log.Printf("AlertEngine: deferred %d alert group(s), first %s - on-call recipients could not be resolved, the next tick retries: %v",
			deferred, deferredFirst, deferredErr)
	}
}

// alertGroupIDs names the batch a tick picked up, capped so that a storm does
// not turn one breadcrumb into one enormous line. The order is the order the
// loop works in, which is what lets a reader narrow a hang down to the groups
// that never reported back. What the cap leaves out is counted, not hidden -
// a reader who sees "and N more" knows the answer may be outside the list.
func alertGroupIDs(ags []*model.AlertGroup) string {
	const shown = 10

	var b strings.Builder
	for i, ag := range ags {
		if i == shown {
			fmt.Fprintf(&b, " (and %d more)", len(ags)-i)
			break
		}
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(ag.ID)
	}
	return b.String()
}

// onCallSnapshot records who was on duty when the alert arrived.
//
// It takes the projection rather than fetching one: this is the same answer the
// job was built from, and that is the whole point of it being a parameter.
//
// A team with no schedule, or one between shifts, gets an empty snapshot rather
// than none: "nobody was on call" is a fact worth having on the alert group, and
// the readers of the field already treat an empty group as exactly that.
//
// Source is what survives of the override information now that the projection
// answers instead of a legacy override row: L1Users already names the stand-in,
// and Source says that is why.
func (e *Engine) onCallSnapshot(team schedulerender.TeamOnCall) (*model.OnCallResult, error) {
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
