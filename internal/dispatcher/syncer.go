package dispatcher

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/scheduler"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/slack-go/slack"
)

// cacheTTL defines how long a cache entry is valid before forcing a refresh.
const cacheTTL = 1 * time.Hour

// cacheEntry stores cached Slack user IDs with timestamp for TTL.
type cacheEntry struct {
	slackIDs  string // sorted comma-joined Slack user IDs
	updatedAt time.Time
}

// UsergroupSyncer synchronizes Slack usergroups with current on-call users.
type UsergroupSyncer struct {
	store        store.StoreInterface
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
	store        store.StoreInterface
	syncInterval time.Duration

	mu           sync.Mutex
	cancel       context.CancelFunc // non-nil means syncer is running (or stopping)
	currentToken string             // track current token to avoid unnecessary restarts
	generation   uint64             // incremented on each Start, used for cleanup
}

// NewUsergroupSyncerManager creates a new manager for UsergroupSyncer.
func NewUsergroupSyncerManager(st store.StoreInterface, interval time.Duration) *UsergroupSyncerManager {
	return &UsergroupSyncerManager{
		store:        st,
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
	syncer := NewUsergroupSyncer(m.store, slackToken, m.syncInterval)

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
func NewUsergroupSyncer(st store.StoreInterface, slackToken string, interval time.Duration) *UsergroupSyncer {
	return &UsergroupSyncer{
		store:        st,
		slackClient:  slack.New(slackToken),
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

// SyncAll syncs all schedules that have a usergroup configured.
func (s *UsergroupSyncer) SyncAll(ctx context.Context) error {
	schedules, err := s.store.GetSchedulesWithUsergroup()
	if err != nil {
		return err
	}

	for _, schedule := range schedules {
		if err := s.SyncSchedule(ctx, schedule); err != nil {
			log.Printf("[UsergroupSyncer] Failed to sync schedule %s: %v", schedule.ID, err)
		}
	}
	return nil
}

// SyncSchedule syncs a single schedule's usergroup with the current on-call user.
func (s *UsergroupSyncer) SyncSchedule(ctx context.Context, schedule *model.Schedule) error {
	now := time.Now().UTC()

	l1Epochs, err := s.store.GetRotationEpochs(schedule.ID, "l1", now, now.Add(32*24*time.Hour))
	if err != nil {
		return err
	}
	if len(l1Epochs) == 0 {
		log.Printf("[UsergroupSyncer] No L1 epochs for schedule %s (team %s), skipping", schedule.ID, schedule.TeamID)
		return nil
	}

	overrides, err := s.store.GetScheduleOverrides(schedule.ID, now, now.Add(32*24*time.Hour))
	if err != nil {
		return err
	}

	// Build user map
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

	fetchedUsers, err := s.store.GetUsersByIDs(uniqueIDs)
	if err != nil {
		return err
	}
	users := make(map[string]*model.User)
	for _, u := range fetchedUsers {
		users[u.ID] = u
	}

	result := scheduler.GetCurrentOnCall(schedule, l1Epochs, nil, overrides, users, now)

	if len(result.L1Users) == 0 {
		log.Printf("[UsergroupSyncer] No L1 users for schedule %s, skipping", schedule.ID)
		return nil
	}

	// Batch-fetch Slack external identities for the L1 group; users without one are skipped.
	l1UserIDs := make([]string, 0, len(result.L1Users))
	for _, u := range result.L1Users {
		l1UserIDs = append(l1UserIDs, u.ID)
	}
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
	for _, u := range result.L1Users {
		ext := slackByUser[u.ID]
		if ext == "" {
			log.Printf("[UsergroupSyncer] WARN: L1 user %s (%s) has no slack identity, skipping",
				u.Name, u.Email)
			continue
		}
		slackIDs = append(slackIDs, ext)
	}
	if len(slackIDs) == 0 {
		log.Printf("[UsergroupSyncer] No L1 users with slack identity for schedule %s, cannot sync usergroup %s",
			schedule.ID, schedule.SlackUsergroupID)
		return nil
	}

	sort.Strings(slackIDs)
	joinedIDs := strings.Join(slackIDs, ",")

	// Check cache - skip if unchanged and not expired
	if cached, ok := s.getCache(schedule.SlackUsergroupID); ok {
		if cached.slackIDs == joinedIDs && time.Since(cached.updatedAt) < cacheTTL {
			return nil
		}
	}

	// Update Slack usergroup with all on-call users
	_, err = s.slackClient.UpdateUserGroupMembersContext(ctx, schedule.SlackUsergroupID, joinedIDs)
	if err != nil {
		log.Printf("[UsergroupSyncer] Failed to update usergroup %s: %v", schedule.SlackUsergroupID, err)
		return err
	}

	s.setCache(schedule.SlackUsergroupID, joinedIDs)
	log.Printf("[UsergroupSyncer] Updated usergroup %s with users %s",
		schedule.SlackUsergroupID, joinedIDs)

	return nil
}
