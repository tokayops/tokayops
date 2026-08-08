package schedulerender

import (
	"context"
	"errors"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Service loads what the pure renderer needs and hands back its answer.
//
// Every load happens inside one snapshot. That is not tidiness: a render is
// built from the root, the revisions and the override projection, and a Save
// committing between those reads would produce an answer describing no state
// that ever existed.
type Service struct {
	repo scheduleconfig.ScheduleReadRepository
	now  func() time.Time
}

// Option customizes a Service.
type Option func(*Service)

// WithClock overrides the wall clock the preview evaluates against.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

func New(repo scheduleconfig.ScheduleReadRepository, opts ...Option) *Service {
	s := &Service{
		repo: repo,
		now:  func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// RenderRange answers who was on duty across [from, until).
//
// asOf selects the system time at which override state is read: nil means as
// it stands now, a value replays the state as it was known then.
//
// The range is normalized to database resolution before anything is fetched.
// Passing it raw would let the queries floor `until` while the renderer clips
// with the nanosecond-precise value, and a revision overlapping the range by
// less than a microsecond would be dropped by the query yet expected by the
// renderer. Result.From/Until report the range that was actually answered.
func (s *Service) RenderRange(ctx context.Context, scheduleID string, from, until time.Time, asOf *time.Time) (Result, error) {
	from = scheduleconfig.NormalizeTimestamp(from)
	until = scheduleconfig.NormalizeTimestamp(until)

	var res Result
	err := s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := view.GetScheduleRoot(ctx, scheduleID)
		if err != nil {
			return err
		}
		revisions, err := view.GetRevisionsInRange(ctx, scheduleID, from, until)
		if err != nil {
			return err
		}
		overrides, err := view.GetOverrideProjectionInRange(ctx, scheduleID, &from, &until, asOf)
		if err != nil {
			return err
		}

		res, err = Render(Input{
			Root:      *root,
			Revisions: revisions,
			Overrides: overrides,
			From:      from,
			Until:     until,
		})
		return err
	})
	if err != nil {
		return Result{}, err
	}
	return res, nil
}

// CurrentOnCall answers who is on duty at one instant.
//
// It deliberately does not render a range: only the slot containing `at` is
// needed, and computing it directly keeps the hot path to a single position
// calculation.
//
// A schedule that does not exist, has no revision covering `at`, or was
// deleted before it yields an empty projection rather than an error. A
// dispatcher asking who to page must be told "nobody", not handed a failure
// it has to interpret.
func (s *Service) CurrentOnCall(ctx context.Context, scheduleID string, at time.Time) (OnCall, error) {
	// Normalized for the same reason the render range is: the query that
	// picks the effective revision floors `at`, so a sub-microsecond instant
	// would be resolved against one revision and have its slot computed at
	// another moment.
	at = scheduleconfig.NormalizeTimestamp(at)

	var out OnCall
	err := s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		var err error
		out, err = onCallWithin(ctx, view, scheduleID, at)
		return err
	})
	if err != nil {
		return OnCall{}, err
	}
	return out, nil
}

// CurrentOnCallNow is the same projection at the service's own clock.
//
// It exists so a caller that means "right now" does not have to reach for
// time.Now() itself. That would be a second clock: the preview already
// evaluates against s.now, and a handler answering from wall time would drift
// from it under WithClock - silently in production, and visibly in any test
// that moves time forward.
func (s *Service) CurrentOnCallNow(ctx context.Context, scheduleID string) (OnCall, error) {
	return s.CurrentOnCall(ctx, scheduleID, s.now().UTC())
}

// onCallWithin is the projection itself, over a view the caller already holds.
// The preview needs the same answer from inside its own snapshot, and computing
// it twice would be two chances to project one state differently.
func onCallWithin(ctx context.Context, view scheduleconfig.ScheduleReadView,
	scheduleID string, at time.Time) (OnCall, error) {

	out := OnCall{At: at}
	rev, err := view.GetEffectiveRevision(ctx, scheduleID, at)
	if errors.Is(err, scheduleconfig.ErrRevisionNotFound) || errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
		return out, nil
	}
	if err != nil {
		return OnCall{}, err
	}
	if rev.Kind == scheduleconfig.RevisionDeleted {
		return out, nil
	}
	return onCallOfRevision(ctx, view, *rev, at)
}

// onCallOfRevision projects one revision - stored or hypothetical - against
// the override state of the same snapshot, fetching exactly the overrides the
// layers' slots can be affected by.
func onCallOfRevision(ctx context.Context, view scheduleconfig.ScheduleReadView,
	rev scheduleconfig.ScheduleRevision, at time.Time) (OnCall, error) {

	slots, err := onCallSlots(rev, at)
	if err != nil {
		return OnCall{}, err
	}
	from, until, ok := onCallOverrideRange(slots)
	if !ok {
		return OnCall{At: at}, nil
	}
	overrides, err := view.GetOverrideProjectionInRange(ctx, rev.ScheduleID, &from, &until, nil)
	if err != nil {
		return OnCall{}, err
	}
	return projectOnCall(rev, at, slots, overrides), nil
}

// projectRevisionOnCall is the same projection against overrides the caller
// already holds. It exists for the preview of a schedule that does not exist
// yet, which has none and therefore has nothing to query for.
func projectRevisionOnCall(rev scheduleconfig.ScheduleRevision, at time.Time,
	overrides []scheduleconfig.OverrideRevision) (OnCall, error) {

	slots, err := onCallSlots(rev, at)
	if err != nil {
		return OnCall{}, err
	}
	return projectOnCall(rev, at, slots, overrides), nil
}
