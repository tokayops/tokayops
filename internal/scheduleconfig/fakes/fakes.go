// Package fakes provides in-memory doubles of the schedule configuration and
// erasure unit-of-work interfaces for service unit tests.
//
// These fakes deliberately mirror only the narrow interfaces, not the full
// persistence model: the legacy MockStore is never extended with revisions.
// SQL correctness is proven by integration tests against a real PostgreSQL.
package fakes

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/tokayops/tokayops/internal/erasure"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// ScheduleConfigRepo is an in-memory scheduleconfig.ScheduleConfigRepository.
//
// It models transaction semantics: state is snapshotted when WithinTx starts
// and restored when the callback returns an error, so a test that injects a
// failure mid-flow observes the same all-or-nothing outcome the store gives.
type ScheduleConfigRepo struct {
	mu    sync.Mutex
	state fakeState

	// FailOn maps a method name to the error it should return instead of
	// running. "WithinTx" fails before the callback; "Commit" fails after it
	// returned successfully.
	FailOn map[string]error

	// Calls records method names in order, including the failing one.
	Calls []string
}

type fakeState struct {
	roots     map[string]scheduleconfig.ScheduleRoot
	teamIndex map[string]string
	revisions map[string][]scheduleconfig.ScheduleRevision
	overrides map[string][]scheduleconfig.OverrideRevision
	events    map[string][]scheduleconfig.ScheduleEvent
}

func newState() fakeState {
	return fakeState{
		roots:     map[string]scheduleconfig.ScheduleRoot{},
		teamIndex: map[string]string{},
		revisions: map[string][]scheduleconfig.ScheduleRevision{},
		overrides: map[string][]scheduleconfig.OverrideRevision{},
		events:    map[string][]scheduleconfig.ScheduleEvent{},
	}
}

// clone copies deeply enough for rollback: slice elements are copied by value
// and the fake only ever replaces whole elements, never mutates through a
// shared pointer.
func (s fakeState) clone() fakeState {
	c := newState()
	for k, v := range s.roots {
		c.roots[k] = v
	}
	for k, v := range s.teamIndex {
		c.teamIndex[k] = v
	}
	for k, v := range s.revisions {
		c.revisions[k] = append([]scheduleconfig.ScheduleRevision(nil), v...)
	}
	for k, v := range s.overrides {
		c.overrides[k] = append([]scheduleconfig.OverrideRevision(nil), v...)
	}
	for k, v := range s.events {
		c.events[k] = append([]scheduleconfig.ScheduleEvent(nil), v...)
	}
	return c
}

// NewScheduleConfigRepo returns an empty repository.
func NewScheduleConfigRepo() *ScheduleConfigRepo {
	return &ScheduleConfigRepo{state: newState(), FailOn: map[string]error{}}
}

func (r *ScheduleConfigRepo) record(method string) error {
	r.Calls = append(r.Calls, method)
	return r.FailOn[method]
}

// WithinTx runs fn against the in-memory state, rolling back on error.
func (r *ScheduleConfigRepo) WithinTx(ctx context.Context, fn func(scheduleconfig.ScheduleConfigTx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.record("WithinTx"); err != nil {
		return err
	}
	before := r.state.clone()
	if err := fn(&scheduleConfigTx{repo: r}); err != nil {
		r.state = before
		return err
	}
	if err := r.record("Commit"); err != nil {
		r.state = before
		return err
	}
	return nil
}

// Root returns a stored schedule root, if present.
func (r *ScheduleConfigRepo) Root(scheduleID string) (scheduleconfig.ScheduleRoot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	root, ok := r.state.roots[scheduleID]
	return root, ok
}

// RootByTeam returns the schedule root owned by a team, if present.
func (r *ScheduleConfigRepo) RootByTeam(teamID string) (scheduleconfig.ScheduleRoot, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.state.teamIndex[teamID]
	if !ok {
		return scheduleconfig.ScheduleRoot{}, false
	}
	root, ok := r.state.roots[id]
	return root, ok
}

// Revisions returns the revisions of a schedule ordered by version.
func (r *ScheduleConfigRepo) Revisions(scheduleID string) []scheduleconfig.ScheduleRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]scheduleconfig.ScheduleRevision(nil), r.state.revisions[scheduleID]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// OverrideRevisions returns every override revision of a schedule in insert
// order.
func (r *ScheduleConfigRepo) OverrideRevisions(scheduleID string) []scheduleconfig.OverrideRevision {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]scheduleconfig.OverrideRevision(nil), r.state.overrides[scheduleID]...)
}

// Events returns the schedule events recorded for a schedule.
func (r *ScheduleConfigRepo) Events(scheduleID string) []scheduleconfig.ScheduleEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]scheduleconfig.ScheduleEvent(nil), r.state.events[scheduleID]...)
}

// RootCount reports how many schedule roots exist.
func (r *ScheduleConfigRepo) RootCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.state.roots)
}

type scheduleConfigTx struct {
	repo *ScheduleConfigRepo
}

func (t *scheduleConfigTx) CreateInitialSchedule(ctx context.Context, root *scheduleconfig.ScheduleRoot, initial *scheduleconfig.ScheduleRevision) error {
	if err := t.repo.record("CreateInitialSchedule"); err != nil {
		return err
	}
	if root == nil || initial == nil {
		return fmt.Errorf("%w: nil root or revision", scheduleconfig.ErrInvariantViolation)
	}
	if initial.ScheduleID != root.ID || initial.Version != 1 || initial.EffectiveTo != nil {
		return fmt.Errorf("%w: initial revision does not describe the root", scheduleconfig.ErrInvariantViolation)
	}
	s := t.repo.state
	if _, exists := s.teamIndex[root.TeamID]; exists {
		return scheduleconfig.ErrScheduleExists
	}
	if _, exists := s.roots[root.ID]; exists {
		return fmt.Errorf("%w: duplicate schedule id %q", scheduleconfig.ErrInvariantViolation, root.ID)
	}

	from := initial.EffectiveFrom
	root.ConfigVersion = 1
	root.HistoryCompleteFrom = &from

	s.roots[root.ID] = *root
	s.teamIndex[root.TeamID] = root.ID
	s.revisions[root.ID] = append(s.revisions[root.ID], *initial)
	return nil
}

func (t *scheduleConfigTx) LockSchedule(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRoot, error) {
	if err := t.repo.record("LockSchedule"); err != nil {
		return nil, err
	}
	root, ok := t.repo.state.roots[scheduleID]
	if !ok {
		return nil, scheduleconfig.ErrScheduleNotFound
	}
	return &root, nil
}

func (t *scheduleConfigTx) GetEffectiveRevision(ctx context.Context, scheduleID string, at time.Time) (*scheduleconfig.ScheduleRevision, error) {
	if err := t.repo.record("GetEffectiveRevision"); err != nil {
		return nil, err
	}
	for _, rev := range t.repo.state.revisions[scheduleID] {
		if rev.EffectiveFrom.After(at) {
			continue
		}
		if rev.EffectiveTo != nil && !rev.EffectiveTo.After(at) {
			continue
		}
		found := rev
		return &found, nil
	}
	return nil, scheduleconfig.ErrRevisionNotFound
}

func (t *scheduleConfigTx) GetTailRevision(ctx context.Context, scheduleID string) (*scheduleconfig.ScheduleRevision, error) {
	if err := t.repo.record("GetTailRevision"); err != nil {
		return nil, err
	}
	for _, rev := range t.repo.state.revisions[scheduleID] {
		if rev.EffectiveTo == nil {
			found := rev
			return &found, nil
		}
	}
	return nil, scheduleconfig.ErrRevisionNotFound
}

func (t *scheduleConfigTx) CloseRevision(ctx context.Context, scheduleID, expectedRevisionID string, at time.Time) error {
	if err := t.repo.record("CloseRevision"); err != nil {
		return err
	}
	revs := t.repo.state.revisions[scheduleID]
	for i, rev := range revs {
		if rev.ID != expectedRevisionID || rev.EffectiveTo != nil {
			continue
		}
		if !at.After(rev.EffectiveFrom) {
			return fmt.Errorf("%w: closing at %v would not follow %v", scheduleconfig.ErrInvariantViolation, at, rev.EffectiveFrom)
		}
		closedAt := at
		revs[i].EffectiveTo = &closedAt
		return nil
	}
	return scheduleconfig.ErrRevisionMismatch
}

func (t *scheduleConfigTx) InsertRevision(ctx context.Context, revision *scheduleconfig.ScheduleRevision) error {
	if err := t.repo.record("InsertRevision"); err != nil {
		return err
	}
	if revision == nil {
		return fmt.Errorf("%w: nil revision", scheduleconfig.ErrInvariantViolation)
	}
	s := t.repo.state
	if _, ok := s.roots[revision.ScheduleID]; !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	for _, existing := range s.revisions[revision.ScheduleID] {
		if existing.Version == revision.Version {
			return fmt.Errorf("%w: duplicate revision version %d", scheduleconfig.ErrInvariantViolation, revision.Version)
		}
		if existing.EffectiveTo == nil && revision.EffectiveTo == nil {
			return fmt.Errorf("%w: schedule would have two open revisions", scheduleconfig.ErrInvariantViolation)
		}
	}
	s.revisions[revision.ScheduleID] = append(s.revisions[revision.ScheduleID], *revision)
	return nil
}

func (t *scheduleConfigTx) AdvanceVersion(ctx context.Context, scheduleID string, expected int64, at time.Time) error {
	if err := t.repo.record("AdvanceVersion"); err != nil {
		return err
	}
	root, ok := t.repo.state.roots[scheduleID]
	if !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	if root.ConfigVersion != expected {
		return scheduleconfig.ErrVersionConflict
	}
	root.ConfigVersion++
	t.repo.state.roots[scheduleID] = root
	return nil
}

func (t *scheduleConfigTx) InsertScheduleEvent(ctx context.Context, event *scheduleconfig.ScheduleEvent) error {
	if err := t.repo.record("InsertScheduleEvent"); err != nil {
		return err
	}
	if event == nil {
		return nil
	}
	s := t.repo.state
	if _, ok := s.roots[event.ScheduleID]; !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	s.events[event.ScheduleID] = append(s.events[event.ScheduleID], *event)
	return nil
}

// GetCurrentOverrides projects the latest revision per override_id and only
// then drops tombstones - filtering first would resurrect the revision that
// preceded a delete.
func (t *scheduleConfigTx) GetCurrentOverrides(ctx context.Context, scheduleID string) ([]scheduleconfig.OverrideRevision, error) {
	if err := t.repo.record("GetCurrentOverrides"); err != nil {
		return nil, err
	}
	latest := map[string]scheduleconfig.OverrideRevision{}
	for _, rev := range t.repo.state.overrides[scheduleID] {
		if cur, ok := latest[rev.OverrideID]; !ok || rev.Revision > cur.Revision {
			latest[rev.OverrideID] = rev
		}
	}
	out := make([]scheduleconfig.OverrideRevision, 0, len(latest))
	for _, rev := range latest {
		if rev.Deleted {
			continue
		}
		out = append(out, rev)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ValidFrom.Equal(out[j].ValidFrom) {
			return out[i].ValidFrom.Before(out[j].ValidFrom)
		}
		return out[i].OverrideID < out[j].OverrideID
	})
	return out, nil
}

func (t *scheduleConfigTx) InsertOverrideRevision(ctx context.Context, rev *scheduleconfig.OverrideRevision) error {
	if err := t.repo.record("InsertOverrideRevision"); err != nil {
		return err
	}
	if rev == nil {
		return fmt.Errorf("%w: nil override revision", scheduleconfig.ErrInvariantViolation)
	}
	s := t.repo.state
	if _, ok := s.roots[rev.ScheduleID]; !ok {
		return scheduleconfig.ErrScheduleNotFound
	}
	for _, existing := range s.overrides[rev.ScheduleID] {
		if existing.OverrideID == rev.OverrideID && existing.Revision == rev.Revision {
			return fmt.Errorf("%w: duplicate override revision %d", scheduleconfig.ErrInvariantViolation, rev.Revision)
		}
	}
	s.overrides[rev.ScheduleID] = append(s.overrides[rev.ScheduleID], *rev)
	return nil
}

// ErasureRepo is an in-memory erasure.Repository that records what was asked
// of it. Like the config fake it rolls back on error.
type ErasureRepo struct {
	mu sync.Mutex

	// FailOn maps a method name to the error it returns instead of running.
	FailOn map[string]error

	// Calls records "Method:userID" in order.
	Calls []string

	deleted    map[string]time.Time
	anonymized map[string]bool
	wiped      map[string][]string
}

// NewErasureRepo returns an empty erasure repository.
func NewErasureRepo() *ErasureRepo {
	return &ErasureRepo{
		FailOn:     map[string]error{},
		deleted:    map[string]time.Time{},
		anonymized: map[string]bool{},
		wiped:      map[string][]string{},
	}
}

// WithinTx runs fn, discarding every recorded effect if it fails.
func (r *ErasureRepo) WithinTx(ctx context.Context, fn func(erasure.Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.fail("WithinTx"); err != nil {
		return err
	}
	beforeDeleted := map[string]time.Time{}
	for k, v := range r.deleted {
		beforeDeleted[k] = v
	}
	beforeAnon := map[string]bool{}
	for k, v := range r.anonymized {
		beforeAnon[k] = v
	}
	beforeWiped := map[string][]string{}
	for k, v := range r.wiped {
		beforeWiped[k] = append([]string(nil), v...)
	}
	beforeCalls := len(r.Calls)

	if err := fn(&erasureTx{repo: r}); err != nil {
		r.deleted, r.anonymized, r.wiped = beforeDeleted, beforeAnon, beforeWiped
		r.Calls = r.Calls[:beforeCalls]
		return err
	}
	return nil
}

func (r *ErasureRepo) fail(method string) error { return r.FailOn[method] }

func (r *ErasureRepo) record(method, userID string) error {
	r.Calls = append(r.Calls, method+":"+userID)
	return r.FailOn[method]
}

// DeletedAt reports the erasure timestamp recorded for a user.
func (r *ErasureRepo) DeletedAt(userID string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.deleted[userID]
	return at, ok
}

// Anonymized reports whether a user was anonymized.
func (r *ErasureRepo) Anonymized(userID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.anonymized[userID]
}

// Wiped lists the erasure primitives applied to a user, in order.
func (r *ErasureRepo) Wiped(userID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.wiped[userID]...)
}

type erasureTx struct {
	repo *ErasureRepo
}

func (t *erasureTx) SetUserDeletedAt(ctx context.Context, userID string, at time.Time) error {
	if err := t.repo.record("SetUserDeletedAt", userID); err != nil {
		return err
	}
	t.repo.deleted[userID] = at
	return nil
}

func (t *erasureTx) AnonymizeUser(ctx context.Context, userID string) error {
	if err := t.repo.record("AnonymizeUser", userID); err != nil {
		return err
	}
	t.repo.anonymized[userID] = true
	return nil
}

func (t *erasureTx) wipe(method, userID string) error {
	if err := t.repo.record(method, userID); err != nil {
		return err
	}
	t.repo.wiped[userID] = append(t.repo.wiped[userID], method)
	return nil
}

func (t *erasureTx) DeleteUserAPITokens(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserAPITokens", userID)
}

func (t *erasureTx) DeleteUserExternalIdentities(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserExternalIdentities", userID)
}

func (t *erasureTx) DeleteUserLinkTokens(ctx context.Context, userID string) error {
	return t.wipe("DeleteUserLinkTokens", userID)
}

func (t *erasureTx) NullifyOverrideRevisionReasons(ctx context.Context, userID string) error {
	return t.wipe("NullifyOverrideRevisionReasons", userID)
}

func (t *erasureTx) NullifyScheduleRevisionChangeReasons(ctx context.Context, userID string) error {
	return t.wipe("NullifyScheduleRevisionChangeReasons", userID)
}

// Compile-time proof the fakes satisfy the interfaces they double.
var (
	_ scheduleconfig.ScheduleConfigRepository = (*ScheduleConfigRepo)(nil)
	_ scheduleconfig.ScheduleConfigTx         = (*scheduleConfigTx)(nil)
	_ erasure.Repository                      = (*ErasureRepo)(nil)
	_ erasure.Tx                              = (*erasureTx)(nil)
)
