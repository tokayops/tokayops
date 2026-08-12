package schedulerender

import (
	"context"
	"errors"
	"fmt"
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
func (s *Service) RenderRange(ctx context.Context, scheduleID string, from, until time.Time) (Result, error) {
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
		overrides, err := view.GetOverrideProjectionInRange(ctx, scheduleID, &from, &until)
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
// A schedule that does not exist, was deleted before `at`, or did not yet
// exist at `at` yields an empty projection rather than an error. A dispatcher
// asking who to page must be told "nobody", not handed a failure it has to
// interpret. Damage is the exception and does surface: a schedule whose chain
// is broken, or which carries no history horizon at all, has no honest answer
// to give.
func (s *Service) CurrentOnCall(ctx context.Context, scheduleID string, at time.Time) (OnCall, error) {
	// Normalized for the same reason the render range is: the query that
	// picks the effective revision floors `at`, so a sub-microsecond instant
	// would be resolved against one revision and have its slot computed at
	// another moment.
	at = scheduleconfig.NormalizeTimestamp(at)

	var out OnCall
	err := s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := view.GetScheduleRoot(ctx, scheduleID)
		if errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
			out = OnCall{At: at}
			return nil
		}
		if err != nil {
			return err
		}
		out, err = onCallWithin(ctx, view, *root, at)
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

// TeamOnCall is who is on duty for a team, together with what kind of schedule
// produced that answer.
//
// The two travel together because neither is enough alone: a team with no
// schedule, a deleted one and a live one between shifts all put nobody on
// duty, and a caller has to tell them apart to know whether to offer to
// configure one.
type TeamOnCall struct {
	// ScheduleID is empty only when the team has no schedule at all. A
	// schedule that exists but could not be projected does not reach here -
	// that is an error, not an answer.
	ScheduleID string
	DeletedAt  *time.Time
	OnCall     OnCall
}

// CurrentTeamOnCallNow answers both questions from one snapshot.
//
// The reason is cost, not correctness. A snapshot is a real read-only
// repeatable-read transaction, and asking for the root separately made this -
// the most frequently read thing in the product - open two of them per
// request, the first to fetch a single row. One transaction answers both
// halves.
//
// It also puts the rules about absent and deleted schedules in one place. A
// caller that assembled the answer itself needed its own copy of them, and two
// copies of "what a schedule that is not there looks like" is how they drift.
//
// Consistency comes along for free: a delete landing between two separate
// reads could have produced an answer describing no state that ever existed.
// That was never likely - schedules change a few times a year, the window is
// sub-millisecond - and it is not what this is for.
func (s *Service) CurrentTeamOnCallNow(ctx context.Context, teamID string) (TeamOnCall, error) {
	at := scheduleconfig.NormalizeTimestamp(s.now().UTC())

	out := TeamOnCall{OnCall: OnCall{At: at}}
	err := s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		root, err := view.GetScheduleRootByTeam(ctx, teamID)
		// No schedule is an answer, not a failure: the question is who is on
		// duty, and "nobody" is true.
		if errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		out.ScheduleID = root.ID
		out.DeletedAt = root.DeletedAt
		out.OnCall, err = onCallWithin(ctx, view, *root, at)
		return err
	})
	if err != nil {
		return TeamOnCall{}, err
	}
	return out, nil
}

// onCallWithin is the projection itself, over a view the caller already holds.
// The preview needs the same answer from inside its own snapshot, and computing
// it twice would be two chances to project one state differently.
func onCallWithin(ctx context.Context, view scheduleconfig.ScheduleReadView,
	root scheduleconfig.ScheduleRoot, at time.Time) (OnCall, error) {

	out, _, err := onCallOfRoot(ctx, view, root, at)
	return out, err
}

// onCallOfRoot is the projection plus the revision it was read from.
//
// The revision travels back because two of its snapshot fields - the timezone
// and the Slack usergroup - are answers about the schedule that consumers need
// alongside the duty, and they belong to the configuration that was in force at
// `at`. Fetching them separately would be a second read of the same row and,
// worse, a second source of truth for them.
//
// It is also the one place the rules about what "nobody" means live, and there
// are exactly two of them:
//
//   - an instant before the schedule's history horizon predates the schedule;
//   - a deleted-kind revision means the schedule did not exist then.
//
// Everything else that fails to produce a revision is damage and says so. An
// active root with no revision in force is a lost row, not a quiet "nobody":
// answering nobody would page no one and look exactly like an empty rotation.
func onCallOfRoot(ctx context.Context, view scheduleconfig.ScheduleReadView,
	root scheduleconfig.ScheduleRoot, at time.Time) (OnCall, *scheduleconfig.ScheduleRevision, error) {

	out := OnCall{At: at}
	if err := scheduleconfig.RequireInitializedRoot(&root); err != nil {
		return OnCall{}, nil, err
	}
	if at.Before(*root.HistoryCompleteFrom) {
		return out, nil, nil
	}

	rev, err := view.GetEffectiveRevision(ctx, root.ID, at)
	if errors.Is(err, scheduleconfig.ErrRevisionNotFound) || errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
		return OnCall{}, nil, fmt.Errorf("%w: schedule %s at %s", ErrRevisionGap, root.ID, at.Format(time.RFC3339))
	}
	if err != nil {
		return OnCall{}, nil, err
	}
	if rev.Kind == scheduleconfig.RevisionDeleted {
		// The deleted tail carries a copy of the last valid snapshot, which is
		// why its timezone and usergroup are still answerable.
		return out, rev, nil
	}

	out, err = onCallOfRevision(ctx, view, *rev, at)
	if err != nil {
		return OnCall{}, nil, err
	}
	return out, rev, nil
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
	overrides, err := view.GetOverrideProjectionInRange(ctx, rev.ScheduleID, &from, &until)
	if err != nil {
		return OnCall{}, err
	}
	return projectOnCall(rev, at, slots, overrides)
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
	return projectOnCall(rev, at, slots, overrides)
}
