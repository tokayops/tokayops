package slacksync

import (
	"context"
	slackprovider "github.com/tokayops/tokayops/internal/outbound/providers/slack"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"github.com/tokayops/tokayops/internal/metrics"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// cacheTTL defines how long a cache entry is valid before forcing a refresh.
const cacheTTL = 1 * time.Hour

// cacheEntry stores cached Slack user IDs with timestamp for TTL.
type cacheEntry struct {
	slackIDs  string // sorted comma-joined Slack user IDs
	updatedAt time.Time
}

// onCallLister is the slice of the schedule projection a sync needs: who is on
// duty everywhere, right now, read from one database snapshot.
//
// Declared here rather than taken as *schedulerender.Service because the
// revision model is deliberately absent from MockStore, and a narrow interface
// is what keeps these unit tests off PostgreSQL. The handover detector takes
// the same shape from the same projection and declares its own: two consumers
// that happen to need one method are not one role.
type onCallLister interface {
	CurrentOnCallForAllNow(ctx context.Context) (schedulerender.BulkOnCall, error)
}

// identityLookup is the store as a sync needs it: where the people on duty can
// be reached. One method, because that is all a sync touches - and a type of its
// own, because the notifier takes a different set from the same store and each
// says so where it is written.
type identityLookup interface {
	GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error)
}

// UsergroupSyncer synchronizes Slack usergroups with current on-call users.
type UsergroupSyncer struct {
	// Only identities are read from the store: the usergroup and its members
	// come from the projection, and reading either from anywhere else would be a
	// second source of truth for them. The narrow type is what says so without a
	// comment having to.
	store        identityLookup
	oncall       onCallLister
	slackClient  *slack.Client
	syncInterval time.Duration

	cacheMu sync.RWMutex
	cache   map[string]cacheEntry
}

// UsergroupSyncerManager manages the lifecycle of UsergroupSyncer.
// It allows dynamic start/stop/restart when Slack integration changes.
// Start/Stop are non-blocking - old syncer will stop asynchronously when its context is cancelled.
// Uses generation-based cleanup to detect dead syncers and allow restart.
type UsergroupSyncerManager struct {
	store        identityLookup
	oncall       onCallLister
	syncInterval time.Duration

	mu           sync.Mutex
	cancel       context.CancelFunc // non-nil means syncer is running (or stopping)
	currentToken string             // track current token to avoid unnecessary restarts
	generation   uint64             // incremented on each Start, used for cleanup
}

// NewUsergroupSyncerManager creates a new manager for UsergroupSyncer.
func NewUsergroupSyncerManager(st identityLookup, oncall onCallLister, interval time.Duration) *UsergroupSyncerManager {
	return &UsergroupSyncerManager{
		store:        st,
		oncall:       oncall,
		syncInterval: interval,
	}
}

// Start starts the syncer with the given token. If already running with the same token, does nothing.
// If running with a different token (or empty), cancels the old syncer and starts a new one.
// Non-blocking: old syncer stops asynchronously.
// If syncer dies (ctx cancelled externally or panic), cleanup goroutine clears state allowing restart.
func (m *UsergroupSyncerManager) Start(ctx context.Context, slackToken string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// If token is the same and syncer is running, do nothing
	if slackToken == m.currentToken && (slackToken == "" || m.cancel != nil) {
		return
	}

	// Cancel old syncer (it will stop asynchronously)
	if m.cancel != nil {
		m.cancel()
	}

	m.currentToken = slackToken
	m.generation++

	if slackToken == "" {
		m.cancel = nil
		return
	}

	syncerCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	syncer := NewUsergroupSyncer(m.store, m.oncall, slackToken, m.syncInterval)

	gen := m.generation
	go func() {
		syncer.Run(syncerCtx)
		// Syncer finished - clean up if generation still matches
		// (prevents race where old syncer clears new syncer's state)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.generation == gen {
			m.cancel = nil
			// currentToken stays - next Start with same token will restart
		}
	}()
}

// Stop cancels the syncer if running. Non-blocking: syncer stops asynchronously.
func (m *UsergroupSyncerManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.currentToken = ""
}

// IsRunning returns true if the syncer is currently running (or stopping).
func (m *UsergroupSyncerManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancel != nil
}

// NewUsergroupSyncer creates a new usergroup syncer.
func NewUsergroupSyncer(st identityLookup, oncall onCallLister, slackToken string, interval time.Duration) *UsergroupSyncer {
	return &UsergroupSyncer{
		store:        st,
		oncall:       oncall,
		slackClient:  slackprovider.NewClient(slackToken, slackprovider.HTTPTimeout),
		syncInterval: interval,
		cache:        make(map[string]cacheEntry),
	}
}

func (s *UsergroupSyncer) getCache(usergroupID string) (cacheEntry, bool) {
	s.cacheMu.RLock()
	defer s.cacheMu.RUnlock()
	if s.cache == nil {
		return cacheEntry{}, false
	}
	entry, ok := s.cache[usergroupID]
	return entry, ok
}

func (s *UsergroupSyncer) setCache(usergroupID, slackIDs string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = make(map[string]cacheEntry)
	}
	s.cache[usergroupID] = cacheEntry{slackIDs: slackIDs, updatedAt: time.Now()}
}

// Run starts the background sync loop.
func (s *UsergroupSyncer) Run(ctx context.Context) {
	log.Printf("[UsergroupSyncer] Starting with %v interval", s.syncInterval)

	if err := s.SyncAll(ctx); err != nil {
		log.Printf("[UsergroupSyncer] Initial sync error: %v", err)
	}

	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[UsergroupSyncer] Shutting down")
			return
		case <-ticker.C:
			if err := s.SyncAll(ctx); err != nil {
				log.Printf("[UsergroupSyncer] Sync error: %v", err)
			}
		}
	}
}

// SyncAll syncs every schedule that has a usergroup configured.
//
// The usergroup comes from the snapshot of the revision in force, not from a
// column on the schedule row: the editor writes it into the configuration, and
// the row's copy is not maintained on the revision path at all - which is how a
// sync that "succeeded over 0 schedules" was the honest report of a filter on a
// column nothing writes.
func (s *UsergroupSyncer) SyncAll(ctx context.Context) error {
	started := time.Now()
	bulk, err := s.oncall.CurrentOnCallForAllNow(ctx)
	metrics.ScheduleOnCallProjectionDuration.
		WithLabelValues(metrics.ConsumerUsergroupSyncer).Observe(time.Since(started).Seconds())
	if err != nil {
		return err
	}

	for _, failure := range bulk.Failures {
		// A group cannot be synced to a membership that could not be read.
		// Leaving the group as it stands is the conservative end: it holds
		// whoever was on duty when the schedule was last readable.
		log.Printf("[UsergroupSyncer] Schedule %s (team %s) could not be projected (%s): %v",
			failure.ScheduleID, failure.TeamID, failure.Reason, failure.Err)
		metrics.ScheduleOnCallProjectionFailuresTotal.
			WithLabelValues(metrics.ConsumerUsergroupSyncer, string(failure.Reason)).Inc()
	}

	for _, sc := range bulk.Schedules {
		// Deleting a schedule has never emptied its usergroup, and this is not
		// where that changes. To the syncer a deleted schedule is simply
		// nothing to sync; to the handoff notifier the same row is an event,
		// which is why the projection reports it and each consumer decides.
		if sc.DeletedAt != nil || sc.SlackUsergroupID == "" {
			continue
		}
		if err := s.SyncSchedule(ctx, sc); err != nil {
			log.Printf("[UsergroupSyncer] Failed to sync schedule %s: %v", sc.ScheduleID, err)
		}
	}
	return nil
}

// SyncSchedule syncs one schedule's usergroup with its current on-call group.
func (s *UsergroupSyncer) SyncSchedule(ctx context.Context, sc schedulerender.ScheduleOnCall) error {
	if sc.SlackUsergroupID == "" {
		return nil
	}
	if sc.OnCall.L1 == nil || len(sc.OnCall.L1.UserIDs) == 0 {
		log.Printf("[UsergroupSyncer] Nobody on call for schedule %s, skipping", sc.ScheduleID)
		return nil
	}

	// The projection has already overlaid any active override, so this is who
	// is really on duty - there is no override branch to take here.
	l1UserIDs := sc.OnCall.L1.UserIDs

	// Batch-fetch Slack external identities for the L1 group; users without one are skipped.
	identities, err := s.store.GetIdentitiesForUsers(l1UserIDs)
	if err != nil {
		return err
	}
	slackByUser := make(map[string]string, len(identities))
	for uid, list := range identities {
		for _, ei := range list {
			if ei.Provider == "slack" && ei.ExternalID != "" {
				slackByUser[uid] = ei.ExternalID
				break
			}
		}
	}

	var slackIDs []string
	for _, uid := range l1UserIDs {
		ext := slackByUser[uid]
		if ext == "" {
			log.Printf("[UsergroupSyncer] WARN: L1 user %s has no slack identity, skipping", uid)
			continue
		}
		slackIDs = append(slackIDs, ext)
	}
	if len(slackIDs) == 0 {
		log.Printf("[UsergroupSyncer] No L1 users with slack identity for schedule %s, cannot sync usergroup %s",
			sc.ScheduleID, sc.SlackUsergroupID)
		return nil
	}

	sort.Strings(slackIDs)
	joinedIDs := strings.Join(slackIDs, ",")

	// Check cache - skip if unchanged and not expired
	if cached, ok := s.getCache(sc.SlackUsergroupID); ok {
		if cached.slackIDs == joinedIDs && time.Since(cached.updatedAt) < cacheTTL {
			return nil
		}
	}

	// Update Slack usergroup with all on-call users
	_, err = s.slackClient.UpdateUserGroupMembersContext(ctx, sc.SlackUsergroupID, joinedIDs)
	if err != nil {
		log.Printf("[UsergroupSyncer] Failed to update usergroup %s: %v", sc.SlackUsergroupID, err)
		return err
	}

	s.setCache(sc.SlackUsergroupID, joinedIDs)
	log.Printf("[UsergroupSyncer] Updated usergroup %s with users %s",
		sc.SlackUsergroupID, joinedIDs)

	return nil
}
