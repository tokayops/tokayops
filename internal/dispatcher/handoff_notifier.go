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
	"github.com/tokayops/tokayops/internal/scheduler"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/google/uuid"
)

// dmProviderLookup is the slice of the capability registry that
// HandoffNotifier needs: enumerate providers that advertise the "dm" target
// kind. Read-only; defined here so unit tests don't have to spin up a full
// ProviderRegistry.
type dmProviderLookup interface {
	ProvidersSupporting(targetKind string) []string
}

// HandoffNotifier detects on-call changes and creates handoff notification jobs.
type HandoffNotifier struct {
	store         store.StoreInterface
	providers     dmProviderLookup
	checkInterval time.Duration

	cacheMu  sync.RWMutex
	cache    map[string]string // scheduleID -> sorted joined L1 user IDs
	warmedUp bool
}

// NewHandoffNotifier creates a new HandoffNotifier. providers is the
// capability registry view used to filter linked identities down to those
// served by a registered dm-capable provider.
func NewHandoffNotifier(st store.StoreInterface, providers dmProviderLookup, interval time.Duration) *HandoffNotifier {
	return &HandoffNotifier{
		store:         st,
		providers:     providers,
		checkInterval: interval,
		cache:         make(map[string]string),
	}
}

// Run starts the background detection loop.
func (n *HandoffNotifier) Run(ctx context.Context) {
	log.Printf("[HandoffNotifier] Starting with %v interval", n.checkInterval)

	// Warm-up: populate cache without creating jobs.
	// Retry until a full error-free pass completes.
	ticker := time.NewTicker(n.checkInterval)
	defer ticker.Stop()

	for {
		if n.checkAll() {
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
			n.checkAll()
		}
	}
}

// checkAll checks all schedules for on-call changes.
// Returns false if any schedule could not be checked due to a fetch error
// (blocks warm-up completion to prevent false handoff DMs).
func (n *HandoffNotifier) checkAll() bool {
	schedules, err := n.store.GetAllSchedules()
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to get schedules: %v", err)
		return false
	}

	teams, err := n.store.GetAllTeams()
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to get teams: %v", err)
		return false
	}

	teamNames := make(map[string]string, len(teams))
	for _, t := range teams {
		teamNames[t.ID] = t.Name
	}

	allOK := true
	for _, schedule := range schedules {
		if !n.checkSchedule(schedule, teamNames) {
			allOK = false
		}
	}
	return allOK
}

// checkSchedule checks a single schedule for on-call changes.
// Returns false on fetch errors that prevented cache population.
func (n *HandoffNotifier) checkSchedule(schedule *model.Schedule, teamNames map[string]string) bool {
	result, ok := n.resolveCurrentOnCall(schedule)
	if !ok {
		return false
	}
	if len(result.L1Users) == 0 {
		return true
	}

	groupKey := sortedUserIDs(result.L1Users)
	if !n.detectChange(schedule.ID, groupKey) {
		return true
	}

	n.handleHandoff(schedule, result, groupKey, teamNames)
	return true
}

// sortedUserIDs returns a deterministic cache key from a group of users.
func sortedUserIDs(users []*model.User) string {
	ids := make([]string, len(users))
	for i, u := range users {
		ids[i] = u.ID
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// resolveCurrentOnCall fetches epochs/overrides/users and computes the current on-call result.
// Returns (nil result, true) if there's no data; returns (nil, false) on fetch errors.
func (n *HandoffNotifier) resolveCurrentOnCall(schedule *model.Schedule) (*model.OnCallResult, bool) {
	now := time.Now().UTC()

	l1Epochs, err := n.store.GetRotationEpochs(schedule.ID, "l1", now, now.Add(32*24*time.Hour))
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to get epochs for schedule %s: %v", schedule.ID, err)
		return nil, false
	}
	if len(l1Epochs) == 0 {
		return &model.OnCallResult{}, true
	}

	overridesFrom, overridesUntil := scheduler.CurrentOnCallWindow(now)
	overrides, err := n.store.GetScheduleOverrides(schedule.ID, overridesFrom, overridesUntil)
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to get overrides for schedule %s: %v", schedule.ID, err)
		return nil, false
	}

	userIDs := make(map[string]bool)
	for _, epoch := range l1Epochs {
		for _, group := range epoch.Groups {
			for _, uid := range group {
				userIDs[uid] = true
			}
		}
	}
	for _, o := range overrides {
		userIDs[o.UserID] = true
	}

	uniqueIDs := make([]string, 0, len(userIDs))
	for id := range userIDs {
		uniqueIDs = append(uniqueIDs, id)
	}

	fetchedUsers, err := n.store.GetUsersByIDs(uniqueIDs)
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to get users for schedule %s: %v", schedule.ID, err)
		return nil, false
	}
	users := make(map[string]*model.User, len(fetchedUsers))
	for _, u := range fetchedUsers {
		users[u.ID] = u
	}

	return scheduler.GetCurrentOnCall(schedule, l1Epochs, nil, overrides, users, now), true
}

// detectChange compares the current L1 user with the cache.
// Returns true if the user changed (or is new).
func (n *HandoffNotifier) detectChange(scheduleID, userID string) bool {
	n.cacheMu.RLock()
	cached, hasCached := n.cache[scheduleID]
	n.cacheMu.RUnlock()

	return !hasCached || cached != userID
}

// handleHandoff processes a detected on-call change: updates cache during warm-up,
// or creates a notification job for all users in the new on-call group.
func (n *HandoffNotifier) handleHandoff(schedule *model.Schedule, result *model.OnCallResult, groupKey string, teamNames map[string]string) {
	n.cacheMu.RLock()
	warmedUp := n.warmedUp
	n.cacheMu.RUnlock()

	if !warmedUp {
		n.setCache(schedule.ID, groupKey)
		return
	}

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

	userIDs := make([]string, 0, len(result.L1Users))
	for _, u := range result.L1Users {
		userIDs = append(userIDs, u.ID)
	}
	identities, err := n.store.GetIdentitiesForUsers(userIDs)
	if err != nil {
		log.Printf("[HandoffNotifier] Failed to load identities for schedule %s: %v", schedule.ID, err)
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

	var notifiable []*model.User
	for _, u := range result.L1Users {
		if len(usableByUser[u.ID]) == 0 {
			log.Printf("[HandoffNotifier] L1 user %s (%s) has no dm-capable linked identity for schedule %s, skipping",
				u.Name, u.ID, schedule.ID)
			continue
		}
		notifiable = append(notifiable, u)
	}

	if len(notifiable) == 0 {
		n.setCache(schedule.ID, groupKey)
		return
	}

	if !n.createHandoffJob(schedule, result, notifiable, usableByUser, groupKey, teamNames) {
		return // Don't update cache — next tick will retry
	}

	n.setCache(schedule.ID, groupKey)
}

// createHandoffJob builds and persists a handoff notification job with one
// stage and N parallel steps. Sprint 4: N is the total number of usable
// linked identities across all notifiable users — a user linked to both Slack
// and Telegram receives one step per provider. The order of steps within a
// user follows usableByUser (sorted by provider name) so step indices stay
// stable across retries.
//
// Returns true on success (or non-retryable error), false if the job should
// be retried.
func (n *HandoffNotifier) createHandoffJob(schedule *model.Schedule, result *model.OnCallResult, notifiable []*model.User, usableByUser map[string][]model.ExternalIdentity, groupKey string, teamNames map[string]string) bool {
	teamName := teamNames[schedule.TeamID]
	if teamName == "" {
		teamName = schedule.TeamID
	}

	message := formatHandoffMessage(teamName, schedule.Timezone, result.L1Since, result.L1Until)

	dedupKey := fmt.Sprintf("handoff:%s:%s", schedule.ID, groupKey)
	now := time.Now()
	job := &model.Job{
		ID:       uuid.New().String(),
		Type:     "handoff_notify",
		Status:   model.JobStatusPending,
		DedupKey: &dedupKey,
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
	for _, u := range notifiable {
		for _, ei := range usableByUser[u.ID] {
			ei := ei // copy for the closure-free loop body
			stepData := model.HandoffStepData{
				ProviderName: ei.Provider,
				TargetID:     identitySendTarget(&ei),
				Message:      message,
				ScheduleID:   schedule.ID,
				TeamID:       schedule.TeamID,
			}
			stepDataJSON, err := json.Marshal(stepData)
			if err != nil {
				log.Printf("[HandoffNotifier] Failed to marshal step data for schedule %s user %s provider %s: %v",
					schedule.ID, u.ID, ei.Provider, err)
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

	if _, err := n.store.CreateJobWithDedup(job, []*model.JobStage{stage}, steps); err != nil {
		log.Printf("[HandoffNotifier] Failed to create handoff job for schedule %s: %v", schedule.ID, err)
		return false
	}

	names := make([]string, len(notifiable))
	for i, u := range notifiable {
		names[i] = u.Name
	}
	log.Printf("[HandoffNotifier] Handoff detected for schedule %s (team %s): notifying %s",
		schedule.ID, teamName, strings.Join(names, ", "))
	return true
}

func (n *HandoffNotifier) setCache(scheduleID, userID string) {
	n.cacheMu.Lock()
	n.cache[scheduleID] = userID
	n.cacheMu.Unlock()
}

// formatHandoffMessage builds the DM text for a handoff notification.
func formatHandoffMessage(teamName, timezone string, shiftStart, shiftEnd *time.Time) string {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}

	msg := fmt.Sprintf(":mega: You are now on-call for team *%s*.\n\n", teamName)

	if shiftStart != nil {
		msg += fmt.Sprintf(":clock1: *Shift start:* %s (%s)\n", shiftStart.In(loc).Format("Mon Jan 2, 15:04"), timezone)
	}

	if shiftEnd != nil {
		msg += fmt.Sprintf(":clock4: *Shift end:* %s (%s)\n", shiftEnd.In(loc).Format("Mon Jan 2, 15:04"), timezone)
	} else {
		msg += ":clock4: *Shift end:* indefinite\n"
	}

	msg += "\n_Shift end time is current as of now and may change if the schedule is modified._"

	return msg
}
