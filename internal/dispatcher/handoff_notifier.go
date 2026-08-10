package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
	"github.com/google/uuid"
)

// dmProviderLookup is the slice of the capability registry that
// HandoffNotifier needs: enumerate providers that advertise the "dm" target
// kind. Read-only; defined here so unit tests don't have to spin up a full
// ProviderRegistry.
type dmProviderLookup interface {
	ProvidersSupporting(targetKind string) []string
}

// onCallLister is the slice of the schedule projection the notifier needs: who
// is on duty everywhere, right now, read from one database snapshot.
//
// It is declared here rather than taken as *schedulerender.Service because the
// revision model is deliberately absent from the legacy MockStore. A narrow
// interface is what keeps these unit tests off PostgreSQL.
type onCallLister interface {
	CurrentOnCallForAllNow(ctx context.Context) (schedulerender.BulkOnCall, error)
}

// The store side of a tick, named by role rather than taken whole.
//
// The notifier used to hold a store.StoreInterface for these three methods,
// which is a hundred-odd others it never calls - and its test double embedded
// that interface, so nothing showed which three were real and a double that
// implemented none of them still compiled.
type teamDirectory interface {
	GetAllTeams() ([]*model.Team, error)
}

type identityLookup interface {
	GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error)
}

type jobCreator interface {
	CreateJobWithDedup(job *model.Job, stages []*model.JobStage,
		steps []*model.JobStep) (id string, created bool, err error)
}

// notifierStore is the three roles together, so the constructor keeps taking
// one argument and *store.Store still satisfies it structurally.
type notifierStore interface {
	teamDirectory
	identityLookup
	jobCreator
}

// HandoffNotifier detects on-call changes and creates notification jobs.
type HandoffNotifier struct {
	store         notifierStore
	oncall        onCallLister
	providers     dmProviderLookup
	checkInterval time.Duration

	cacheMu sync.RWMutex

	// cache is the last composition observed per schedule, and it has three
	// states, not two: no key at all means the schedule has not been observed
	// in this process, a stored empty composition means it was observed with
	// nobody on duty, and anything else is that composition. Conflating the
	// first two either mass-mails everyone at cutover or goes silent after a
	// delete and recreate.
	//
	// The values are stored by value rather than as pointers precisely so the
	// three states are all the type can express: a nil pointer in here would be
	// a fourth state meaning nothing.
	cache    map[string]composition
	warmedUp bool
}

// NewHandoffNotifier creates a new HandoffNotifier. oncall is the schedule
// projection; providers is the capability registry view used to filter linked
// identities down to those served by a registered dm-capable provider.
func NewHandoffNotifier(st notifierStore, oncall onCallLister, providers dmProviderLookup, interval time.Duration) *HandoffNotifier {
	return &HandoffNotifier{
		store:         st,
		oncall:        oncall,
		providers:     providers,
		checkInterval: interval,
		cache:         make(map[string]composition),
	}
}

// Run starts the background detection loop.
func (n *HandoffNotifier) Run(ctx context.Context) {
	log.Printf("[HandoffNotifier] Starting with %v interval", n.checkInterval)

	// Warm-up: populate the cache without creating jobs. It is retried until a
	// tick completes as a call - see checkAll for why one damaged schedule is
	// not allowed to hold it up.
	ticker := time.NewTicker(n.checkInterval)
	defer ticker.Stop()

	for {
		if n.checkAll(ctx) {
			n.cacheMu.Lock()
			n.warmedUp = true
			n.cacheMu.Unlock()
			log.Println("[HandoffNotifier] Warm-up complete")
			break
		}
		metrics.HandoffWarmupNotComplete.Inc()
		log.Println("[HandoffNotifier] Warm-up incomplete, retrying next tick")
		select {
		case <-ctx.Done():
			log.Println("[HandoffNotifier] Shutting down during warm-up")
			return
		case <-ticker.C:
		}
	}

	for {
		select {
		case <-ctx.Done():
			log.Println("[HandoffNotifier] Shutting down")
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
func (n *HandoffNotifier) checkAll(ctx context.Context) bool {
	bulk, err := n.oncall.CurrentOnCallForAllNow(ctx)
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to project on-call state: %v", err)
		return false
	}

	for _, failure := range bulk.Failures {
		// The cache of a damaged schedule is deliberately left alone. Writing
		// an empty composition would turn corruption into "the duty ended" and,
		// on repair, into a notification nobody earned.
		log.Printf("[HandoffNotifier] Schedule %s (team %s) could not be projected (%s): %v",
			failure.ScheduleID, failure.TeamID, failure.Reason, failure.Err)
		metrics.ScheduleOnCallProjectionFailuresTotal.WithLabelValues(string(failure.Reason)).Inc()
	}

	if len(bulk.Schedules) == 0 {
		return true
	}

	teams, err := n.store.GetAllTeams()
	if err != nil {
		// The team name is only how the message addresses the reader, but a
		// tick that cannot read teams cannot read jobs either; treat it as the
		// read failure it is instead of DMing people about team "t-1234".
		log.Printf("[HandoffNotifier] Failed to get teams: %v", err)
		return false
	}
	teamNames := make(map[string]string, len(teams))
	for _, t := range teams {
		teamNames[t.ID] = t.Name
	}

	for _, sc := range bulk.Schedules {
		n.checkSchedule(sc, teamNames)
	}
	return true
}

// checkSchedule compares one schedule against what was last seen of it.
func (n *HandoffNotifier) checkSchedule(sc schedulerender.ScheduleOnCall, teamNames map[string]string) {
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
		log.Printf("[HandoffNotifier] %s on schedule %s with no newly assigned users, nothing to send",
			kind, sc.ScheduleID)
		n.setCache(sc.ScheduleID, next.Composition)
		return
	}

	n.handleHandoff(sc, kind, notify, next, teamNames)
}

// handleHandoff creates the notification job for one transition.
func (n *HandoffNotifier) handleHandoff(sc schedulerender.ScheduleOnCall, kind string,
	notify []string, next observation, teamNames map[string]string) {

	// Sprint 4 (Epic 7 L7): fan-out per linked identity instead of picking
	// the Slack one. Restrict to providers that (a) are registered and
	// (b) advertise the "dm" target kind — otherwise stale or unrelated
	// identities (e.g. an OIDC sub stored as an identity, or a provider we
	// removed) would create steps that immediately fail.
	dmProviders := map[string]bool{}
	if n.providers != nil {
		for _, p := range n.providers.ProvidersSupporting("dm") {
			dmProviders[p] = true
		}
	}

	identities, err := n.store.GetIdentitiesForUsers(notify)
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to load identities for schedule %s: %v", sc.ScheduleID, err)
		return // Don't cache — next tick retries
	}

	// Per-user list of usable identities, ordered by provider name for
	// stable step indices across ticks.
	usableByUser := make(map[string][]model.ExternalIdentity, len(identities))
	for uid, list := range identities {
		var keep []model.ExternalIdentity
		for _, ei := range list {
			if ei.ExternalID == "" || !dmProviders[ei.Provider] {
				continue
			}
			keep = append(keep, *ei)
		}
		sort.Slice(keep, func(i, j int) bool { return keep[i].Provider < keep[j].Provider })
		if len(keep) > 0 {
			usableByUser[uid] = keep
		}
	}

	var notifiable []string
	for _, uid := range notify {
		if len(usableByUser[uid]) == 0 {
			log.Printf("[HandoffNotifier] User %s has no dm-capable linked identity for schedule %s, skipping",
				uid, sc.ScheduleID)
			continue
		}
		notifiable = append(notifiable, uid)
	}

	if len(notifiable) == 0 {
		n.setCache(sc.ScheduleID, next.Composition)
		return
	}

	if !n.createHandoffJob(sc, kind, notifiable, usableByUser, next, teamNames) {
		return // Don't update cache — next tick will retry
	}

	n.setCache(sc.ScheduleID, next.Composition)
}

// createHandoffJob builds and persists a notification job with one stage and N
// parallel steps. Sprint 4: N is the total number of usable linked identities
// across all notified users — a user linked to both Slack and Telegram receives
// one step per provider. The order of steps within a user follows usableByUser
// (sorted by provider name) so step indices stay stable across retries.
//
// Returns true on success (or non-retryable error), false if the job should
// be retried.
func (n *HandoffNotifier) createHandoffJob(sc schedulerender.ScheduleOnCall, kind string,
	notifiable []string, usableByUser map[string][]model.ExternalIdentity,
	next observation, teamNames map[string]string) bool {

	teamName := teamNames[sc.TeamID]
	if teamName == "" {
		teamName = sc.TeamID
	}

	message := formatHandoffMessage(kind, teamName, sc.Timezone, next)

	dedupKey := occurrenceKey(kind, sc.ScheduleID, next)
	now := time.Now()
	job := &model.Job{
		ID:       uuid.New().String(),
		Type:     "handoff_notify",
		Status:   model.JobStatusPending,
		DedupKey: &dedupKey,
		// Stamped like every other job builder does. Left unset - as it was
		// before - the row lands at year zero, and anything ordering jobs by
		// creation time (the job list, GetJobByDedupKey) sees notifications in
		// an order that has nothing to do with when they happened.
		CreatedAt: now,
		UpdatedAt: now,
	}
	stageID := uuid.New().String()
	stage := &model.JobStage{
		ID:         stageID,
		JobID:      job.ID,
		StageIndex: 0,
		Status:     model.JobStageStatusActive,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	var steps []*model.JobStep
	idx := 0
	for _, uid := range notifiable {
		for _, ei := range usableByUser[uid] {
			ei := ei // copy for the closure-free loop body
			stepData := model.HandoffStepData{
				ProviderName: ei.Provider,
				TargetID:     identitySendTarget(&ei),
				Message:      message,
				ScheduleID:   sc.ScheduleID,
				TeamID:       sc.TeamID,
			}
			stepDataJSON, err := json.Marshal(stepData)
			if err != nil {
				log.Printf("[HandoffNotifier] Failed to marshal step data for schedule %s user %s provider %s: %v",
					sc.ScheduleID, uid, ei.Provider, err)
				continue
			}
			steps = append(steps, &model.JobStep{
				ID:                uuid.New().String(),
				JobID:             job.ID,
				StageID:           stageID,
				StepIndex:         idx,
				StepType:          "handoff_notify",
				Status:            model.JobStepStatusPending,
				Data:              stepDataJSON,
				MaxAttempts:       3,
				ContinueOnFailure: true,
			})
			idx++
		}
	}

	if len(steps) == 0 {
		return true
	}

	_, created, err := n.store.CreateJobWithDedup(job, []*model.JobStage{stage}, steps)
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to create %s job for schedule %s: %v", kind, sc.ScheduleID, err)
		return false
	}
	if !created {
		// Another instance detected the same transition first. The notification
		// is on its way; counting it here as well would double a metric that is
		// supposed to say how many were sent.
		log.Printf("[HandoffNotifier] %s job for schedule %s already exists, skipping", kind, sc.ScheduleID)
		return true
	}

	// Counted once per transition, not per step: a person linked to two
	// providers still received one notification.
	metrics.ScheduleOnCallNotificationsTotal.WithLabelValues(kind).Inc()
	log.Printf("[HandoffNotifier] %s detected for schedule %s (team %s): notifying %s",
		kind, sc.ScheduleID, teamName, strings.Join(notifiable, ", "))
	return true
}

// cached returns the composition last observed for a schedule, or nil if the
// schedule has not been observed in this process. The pointer never leaves this
// package and is never stored: it is how "never seen" is told from "seen with
// nobody on duty".
func (n *HandoffNotifier) cached(scheduleID string) *composition {
	n.cacheMu.RLock()
	defer n.cacheMu.RUnlock()
	c, ok := n.cache[scheduleID]
	if !ok {
		return nil
	}
	return &c
}

func (n *HandoffNotifier) setCache(scheduleID string, c composition) {
	n.cacheMu.Lock()
	n.cache[scheduleID] = c.clone()
	n.cacheMu.Unlock()
}

// formatHandoffMessage builds the DM text.
//
// Both kinds carry both pairs of boundaries, each labelled for what it is. For
// an ordinary handoff the first two lines hold the same instant, and that is
// fine: the coincidence is a fact, not a reason to hide one of them. They differ
// exactly where it matters - the shift began at 11:00 and the stand-in's
// assignment starts at 14:00 - and the two kinds differ in their first line:
// one says you came on call, the other that you joined a shift in progress.
//
// There is no "indefinite" case any more. A grid slot always ends, so the
// current assignment always has an end.
func formatHandoffMessage(kind, teamName, timezone string, next observation) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	if timezone == "" {
		timezone = "UTC"
	}
	at := func(t time.Time) string {
		return fmt.Sprintf("%s (%s)", t.In(loc).Format("Mon Jan 2, 15:04"), timezone)
	}

	headline := fmt.Sprintf(":mega: You are now on-call for team *%s*.\n\n", teamName)
	if kind == kindAddedToActiveShift {
		headline = fmt.Sprintf(":heavy_plus_sign: You have been added to the on-call shift in progress for team *%s*.\n\n", teamName)
	}

	return headline +
		fmt.Sprintf(":clock1: Rotation shift started:         %s\n", at(next.GridSlotStart)) +
		fmt.Sprintf(":clock1: Your assignment effective from: %s\n", at(next.AssignmentStart)) +
		fmt.Sprintf(":clock4: Assignment ends:                %s\n", at(next.AssignmentEnd)) +
		"\n_Assignment end is current as of now and may change if the schedule is modified._"
}
