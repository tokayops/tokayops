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
}

func New(repo scheduleconfig.ScheduleReadRepository) *Service {
	return &Service{repo: repo}
}

// RenderRange answers who was on duty across [from, until).
//
// asOf selects the system time at which override state is read: nil means as
// it stands now, a value replays the state as it was known then.
func (s *Service) RenderRange(ctx context.Context, scheduleID string, from, until time.Time, asOf *time.Time) (Result, error) {
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
	out := OnCall{At: at}
	err := s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		rev, err := view.GetEffectiveRevision(ctx, scheduleID, at)
		if errors.Is(err, scheduleconfig.ErrRevisionNotFound) || errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if rev.Kind == scheduleconfig.RevisionDeleted {
			return nil
		}

		slots, err := onCallSlots(*rev, at)
		if err != nil {
			return err
		}
		from, until, ok := onCallOverrideRange(slots)
		if !ok {
			return nil
		}
		overrides, err := view.GetOverrideProjectionInRange(ctx, scheduleID, &from, &until, nil)
		if err != nil {
			return err
		}

		out = projectOnCall(*rev, at, slots, overrides)
		return nil
	})
	if err != nil {
		return OnCall{}, err
	}
	return out, nil
}
