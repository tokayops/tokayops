package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
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
	store  escalationStore
	oncall onCallProjection
	plan   *planner
}

// escalationStore is the store as the escalation engine needs it: find the
// groups nobody has been paged for, learn who to page, and admit the escalation.
//
// SubmitBatch is the one that carries the boundary, and everything the
// engine decides about a group goes through it. Anything written beside it -
// the policy, who was on duty - would be written by producers that lost the
// claim as readily as by the one that won.
type escalationStore interface {
	// GetEscalationSources is the read a plan is built from: the groups nobody
	// has been paged for, with the alerts and the history their cards are drawn
	// from, and the version they were read at - all from one consistent view of
	// the database. A group read here, a history read there and a version read
	// somewhere else could already describe three different alerts.
	GetEscalationSources(ctx context.Context) ([]*model.AlertGroup, error)
	GetTeamByID(id string) (*model.Team, error)
	GetUsersByIDs(ids []string) ([]*model.User, error)

	// Passed down to the escalation builder, which needs the policy this group
	// is escalated by. Listed here because the engine hands its own store to
	// it: whatever the builder may read, the engine may read.
	GetEscalationPolicyByID(id string) (*model.EscalationPolicy, error)

	// SubmitBatch admits the whole escalation in one commit: the
	// claim over the group, its commitments, the state they render from, the
	// policy they were built against and who was on duty when the alert
	// arrived. Split into separate calls, a crash between them leaves a group
	// that looks handled and is not - and a loser of the claim describing an
	// escalation somebody else is running.
	SubmitBatch(ctx context.Context, batch outbound.Batch) (outbound.SubmitResult, error)
}

// NewEngine builds the alert engine. The on-call projection is shared with the
// producer rather than constructed here: who the group records and who it pages
// must be the same answer, and two projections would be two clocks.
//
// The producer is built once, here, rather than per alert group. It holds no
// state between plans - what one plan remembers lives for that plan - so a
// shared instance is the same object the loop was allocating each time round.
func NewEngine(s escalationStore, oncall onCallProjection, settings channelSettings,
	cfg *config.Config) *Engine {

	return &Engine{
		store:  s,
		oncall: oncall,
		plan:   &planner{store: s, oncall: oncall, settings: settings, cfg: cfg},
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
	alertGroups, err := e.store.GetEscalationSources(ctx)
	if err != nil {
		log.Printf("AlertEngine: Error reading the escalation sources: %v", err)
		return
	}
	metrics.EngineAlertGroupsPickedTotal.Add(float64(len(alertGroups)))

	// The tick names the batch before it touches it, because every other line
	// below is written after the work it describes. Without this one, a hang or
	// a panic - anywhere inside the plan, which reads the team, the policy and
	// the schedule - leaves nothing at all to go on, and a poison group in a
	// crash loop cannot be narrowed down.
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

		// What this alert promises, and to whom: the state every message will
		// be rendered from, and one commitment per recipient.
		admission, err := e.plan.buildPlan(ctx, ag, teamOnCall)

		// The recipients could not be resolved, so nothing is admitted and the
		// alert group stays "new" for the next tick to try again. Admitting
		// would spend this alert's only chance to page its on-call: nothing
		// picks up a group that already holds an admission.
		//
		// Nothing is logged for this group: it will be back next tick, and the
		// tick after that. The tally declared above the loop reports all of
		// them in one line once the loop is done.
		if errors.Is(err, ErrOnCallResolutionUnavailable) {
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
		// step plans fine without this answer, and damaged schedule data
		// degrades to a recorded reason rather than deferring. Both admit, so
		// this group is processed once rather than every tick.
		if err := teamOnCall.Err(); err != nil {
			log.Printf("AlertEngine: Failed to fetch oncall for %s: %v", ag.ID, err)
		}

		if err != nil {
			// The plan could not be built - a state that cannot be frozen, a
			// grammar that refuses it. Nothing is admitted and the group stays
			// new, so the next tick tries again rather than spending this
			// alert's only chance to page on a half-built escalation.
			log.Printf("AlertEngine: Failed to build the escalation for %s (will retry): %v", ag.ID, err)
			continue
		}

		result, err := e.store.SubmitBatch(ctx, admission)
		if err != nil {
			log.Printf("AlertEngine: Failed to admit the escalation for %s (will retry): %v", ag.ID, err)
			continue
		}
		metrics.OutboundAdmissionsTotal.WithLabelValues(
			outbound.AdmissionLabel(result.Outcome, len(admission.Admission.Commitments))).Inc()

		if result.Outcome == outbound.SubmitSourceChanged {
			// The alert changed between being read and being admitted, so this
			// plan describes a state it is no longer in. Nothing was claimed;
			// the next tick plans it again from what is now there.
			//
			// Counted rather than logged, like the deferrals above and for the
			// same reason: the group comes straight back, so a line here is a
			// line every tick for as long as the alert keeps moving.
			metrics.EngineEscalationSourceChangedTotal.Inc()
			continue
		}

		// One shape for every other answer, so that "what happened to this
		// alert's escalation" is one grep rather than three phrasings.
		//
		// commitments is what the CLAIM holds, not what this tick planned: on a
		// repeat that is the winner's set, which is the number that matters -
		// how many messages this alert is going to produce. Nobody to notify is
		// an answer rather than a failure, and it reads as commitments=0 with
		// the reasons in the alert's own history.
		about, _ := admission.Context.Escalation()
		log.Printf("AlertEngine: %s admission=%s policy=%s commitments=%d unpromised=%d",
			ag.ID, outbound.AdmissionLabel(result.Outcome, len(admission.Admission.Commitments)),
			about.PolicyID, len(result.IntentIDs), len(about.Unpromised))
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
