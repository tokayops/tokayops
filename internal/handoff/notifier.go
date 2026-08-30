package handoff

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/providers"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// providerLookup is the slice of the capability registry the announcement
// builder needs: what one named provider can do here, and whether this build
// knows it at all.
//
// Per name rather than a list of the dm-capable, because the two ways a
// provider fails to qualify are different things to whoever has to fix them. A
// channel that is registered and does not carry a direct message is configured
// that way; a channel this build has never heard of was taken away. A list of
// the qualifying providers answers neither question - everything absent from it
// looks the same.
//
// Read-only, and defined here so unit tests do not have to spin up a full
// ProviderRegistry.
type providerLookup interface {
	Capabilities(name string) (providers.Capability, bool)
}

// onCallLister is the slice of the schedule projection the notifier needs: who
// is on duty everywhere, right now, read from one database snapshot.
//
// It is declared here rather than taken as *schedulerender.Service because the
// revision model is deliberately absent from MockStore. A narrow
// interface is what keeps these unit tests off PostgreSQL.
type onCallLister interface {
	CurrentOnCallForAllNow(ctx context.Context) (schedulerender.BulkOnCall, error)
}

// notifierStore is the store side of a tick, named by role rather than taken
// whole.
//
// The notifier used to hold a store.StoreInterface for these three methods,
// which is a hundred-odd others it never calls - and its test double embedded
// that interface, so nothing showed which three were real and a double that
// implemented none of them still compiled.
//
// Three methods and one type, though, not three types: a part of a role becomes
// a named interface of its own when something takes that part alone, the way
// the syncer takes identityLookup from the same store. Named without a taker, a
// role is one more jump for the reader and nothing else.
type notifierStore interface {
	// The team name the announcement is about. It travels INTO the payload
	// rather than into a rendered line, so a channel writes it its own way.
	GetAllTeams() ([]*model.Team, error)

	// Where the people coming on duty can be reached, per provider. Read by
	// the builder through addressBook, which is the narrow half of this.
	GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error)

	// SubmitBatch admits the announcement: the claim over the occurrence and
	// every commitment under it, in one commit. Two instances that saw the same
	// shift change offer the same occurrence, and exactly one of them creates
	// the work - which is the same rule the escalation is admitted by, and the
	// reason the answer is an outcome rather than an error.
	SubmitBatch(ctx context.Context, batch outbound.Batch) (outbound.SubmitResult, error)
}

// Notifier detects on-call changes and offers an announcement for each.
type Notifier struct {
	store         notifierStore
	oncall        onCallLister
	build         announcementBuilder
	checkInterval time.Duration

	cacheMu sync.RWMutex

	// cache is the last composition observed per schedule, and it has three
	// states, not two: no key at all means the schedule has not been observed
	// in this process, a stored empty composition means it was observed with
	// nobody on duty, and anything else is that composition. Conflating the
	// first two either mass-mails everyone on the next restart or goes silent
	// after a delete and recreate.
	//
	// The values are stored by value rather than as pointers precisely so the
	// three states are all the type can express: a nil pointer in here would be
	// a fourth state meaning nothing.
	cache    map[string]composition
	warmedUp bool
}

// NewNotifier builds the detector. oncall is the schedule
// projection; providers is the capability registry view used to filter linked
// identities down to those served by a registered dm-capable provider.
func NewNotifier(st notifierStore, oncall onCallLister, catalog providerLookup,
	interval time.Duration) *Notifier {

	return &Notifier{
		store:         st,
		oncall:        oncall,
		build:         announcementBuilder{identities: st, providers: catalog},
		checkInterval: interval,
		cache:         make(map[string]composition),
	}
}

// Run starts the background detection loop.
func (n *Notifier) Run(ctx context.Context) {
	log.Printf("[Notifier] Starting with %v interval", n.checkInterval)

	// Warm-up: populate the cache without announcing anything. It is retried
	// until a tick completes as a call - see checkAll for why one damaged
	// schedule is not allowed to hold it up.
	ticker := time.NewTicker(n.checkInterval)
	defer ticker.Stop()

	for {
		if n.checkAll(ctx) {
			n.cacheMu.Lock()
			n.warmedUp = true
			n.cacheMu.Unlock()
			log.Println("[Notifier] Warm-up complete")
			break
		}
		metrics.HandoffWarmupNotComplete.Inc()
		log.Println("[Notifier] Warm-up incomplete, retrying next tick")
		select {
		case <-ctx.Done():
			log.Println("[Notifier] Shutting down during warm-up")
			return
		case <-ticker.C:
		}
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("[Notifier] Shutting down")
			return
		case <-ticker.C:
			n.checkAll(ctx)
		}
	}
}

// checkAll projects every schedule once and reacts to what changed. It reports
// whether warm-up may be considered complete.
//
// That answer is decided by the CALL, not by its contents. A tick that read the
// database and found one schedule corrupt has still observed every other
// schedule, and refusing to finish warming up would rebuild the very blast
// radius this rewrite removes - one damaged row and nobody, anywhere, is ever
// told they came on call. Only a failure to read at all - the snapshot, the
// list query, the driver - leaves the cache untouched and warm-up pending.
//
// The cost of that is named rather than hidden: a schedule damaged at the moment
// the process started stays unknown, so its first transition after a repair
// passes in silence like any first observation. Marking it "observed with nobody
// on duty" instead would be worse - the repair would then look like a shift
// starting after a gap and DM a group whose duty never changed.
func (n *Notifier) checkAll(ctx context.Context) bool {
	// Observed around the call whether or not it succeeds: a tick that gets
	// slower and then starts failing is the shape this is meant to show.
	started := time.Now()
	bulk, err := n.oncall.CurrentOnCallForAllNow(ctx)
	metrics.ScheduleOnCallProjectionDuration.
		WithLabelValues(metrics.ConsumerHandoffNotifier).Observe(time.Since(started).Seconds())
	if err != nil {
		log.Printf("[Notifier] Failed to project on-call state: %v", err)
		return false
	}

	for _, failure := range bulk.Failures {
		// The cache of a damaged schedule is deliberately left alone. Writing
		// an empty composition would turn corruption into "the duty ended" and,
		// on repair, into a notification nobody earned.
		log.Printf("[Notifier] Schedule %s (team %s) could not be projected (%s): %v",
			failure.ScheduleID, failure.TeamID, failure.Reason, failure.Err)
		metrics.ScheduleOnCallProjectionFailuresTotal.
			WithLabelValues(metrics.ConsumerHandoffNotifier, string(failure.Reason)).Inc()
	}

	if len(bulk.Schedules) == 0 {
		return true
	}

	teams, err := n.store.GetAllTeams()
	if err != nil {
		// The team name is what the announcement is about, but a tick that
		// cannot read teams is a tick that cannot read anything; treat it as
		// the read failure it is instead of telling people they are on call
		// for team "t-1234".
		log.Printf("[Notifier] Failed to get teams: %v", err)
		return false
	}
	teamNames := make(map[string]string, len(teams))
	for _, t := range teams {
		teamNames[t.ID] = t.Name
	}

	for _, sc := range bulk.Schedules {
		n.checkSchedule(ctx, sc, teamNames)
	}
	return true
}

// checkSchedule compares one schedule against what was last seen of it.
func (n *Notifier) checkSchedule(ctx context.Context, sc schedulerender.ScheduleOnCall,
	teamNames map[string]string) {

	next := observe(sc)
	kind, notify := classify(n.cached(sc.ScheduleID), next)

	n.cacheMu.RLock()
	warmedUp := n.warmedUp
	n.cacheMu.RUnlock()

	if !warmedUp || kind == "" {
		n.setCache(sc.ScheduleID, next.Composition)
		return
	}
	if len(notify) == 0 {
		// A real transition with nobody new on it: the group shrank, or the
		// override handed duty back to people who were already in it.
		log.Printf("[Notifier] %s on schedule %s with no newly assigned users, nothing to send",
			kind, sc.ScheduleID)
		n.setCache(sc.ScheduleID, next.Composition)
		return
	}

	n.handleHandoff(ctx, sc, kind, notify, next, teamNames)
}

// handleHandoff admits the announcement for one transition, and decides from
// the answer whether this schedule has been dealt with.
//
// Moving the cache is what makes a transition observed rather than pending, so
// the rule is a closed one and not "after a success". Two of the six answers
// are successes that mean the work exists - ours or somebody else's - and one
// is a failure that no repeat can fix. All three end the schedule's business
// here. The rest leave the cache alone, and the next tick sees the same
// transition again.
func (n *Notifier) handleHandoff(ctx context.Context, sc schedulerender.ScheduleOnCall,
	kind string, notify []string, next observation, teamNames map[string]string) {

	teamName := teamNames[sc.TeamID]
	if teamName == "" {
		teamName = sc.TeamID
	}

	batch, left, err := n.build.build(sc, kind, notify, next, teamName)
	if err != nil {
		// Nothing is known and nothing is claimed. The cache stays where it
		// was so the next tick asks again: an occurrence admitted with nobody
		// on it because a read failed would be a durable "there was nobody to
		// tell", and the real announcement could never be made afterwards.
		log.Printf("[Notifier] Could not build the %s announcement for schedule %s (will retry): %v",
			kind, sc.ScheduleID, err)
		return
	}

	result, err := n.store.SubmitBatch(ctx, batch)
	if err != nil {
		log.Printf("[Notifier] Failed to admit the %s announcement for schedule %s (will retry): %v",
			kind, sc.ScheduleID, err)
		return
	}
	promised := len(batch.Admission.Commitments)
	label := outbound.AdmissionLabel(result.Outcome, promised)

	switch result.Outcome {
	case outbound.SubmitCreated:
		// This instance is the one that made the promise, so this is the one
		// place the people it left out are counted. The loser of the race sees
		// exactly the same unreachable people, and counting there too would
		// report one person missed as two.
		metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kind).Inc()
		for _, s := range left {
			metrics.HandoffRecipientsSkippedTotal.WithLabelValues(string(s.Reason)).Inc()
			log.Printf("[Notifier] %s on schedule %s: %s was not announced to (%s)",
				kind, sc.ScheduleID, s.UserID, s.Reason)
		}

	case outbound.SubmitExisting:
		// Somebody else admitted this occurrence, or we did and lost the
		// answer. Either way it is held, and held correctly.

	case outbound.SubmitConflict:
		// Two instances built the same occurrence with different sets - live
		// configuration that differed by a field between them, say. The winner
		// already holds the claim and its announcement is already going out;
		// there is nothing to ask and nobody to overrule, so a repeat would
		// only produce this same answer every tick until the shift ended. The
		// line naming both fingerprints is written by the store, which is the
		// only place that has them both.
		log.Printf("[Notifier] %s on schedule %s is held by a different announcement; moving on",
			kind, sc.ScheduleID)

	case outbound.SubmitRecipientErased:
		// The set of people this was about is not the set that exists any
		// more. The next tick projects it again.
		log.Printf("[Notifier] %s on schedule %s names an erased recipient; the next tick rebuilds it",
			kind, sc.ScheduleID)
		return

	default:
		// source_changed and group_not_admitted are answers about an alert
		// group, and a handover has none. Reaching one means the store took
		// this announcement down a branch that is not about it at all, so the
		// schedule is left pending rather than marked done on an answer to
		// somebody else's question.
		log.Printf("[Notifier] %s on schedule %s was answered %q, which is not an answer about a shift change",
			kind, sc.ScheduleID, result.Outcome)
		return
	}

	log.Printf("[Notifier] %s on schedule %s (team %s) admission=%s promised=%d skipped=%d",
		kind, sc.ScheduleID, teamName, label, promised, len(left))
	n.setCache(sc.ScheduleID, next.Composition)
}

// cached returns the composition last observed for a schedule, or nil if the
// schedule has not been observed in this process. The pointer never leaves this
// package and is never stored: it is how "never seen" is told from "seen with
// nobody on duty".
func (n *Notifier) cached(scheduleID string) *composition {
	n.cacheMu.RLock()
	defer n.cacheMu.RUnlock()
	c, ok := n.cache[scheduleID]
	if !ok {
		return nil
	}
	return &c
}

func (n *Notifier) setCache(scheduleID string, c composition) {
	n.cacheMu.Lock()
	n.cache[scheduleID] = c.clone()
	n.cacheMu.Unlock()
}
