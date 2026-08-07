package scheduleconfig

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/tokayops/tokayops/internal/rotation"
)

// Service is the only application entry point for schedule configuration
// commands. HTTP handlers bind and map errors; they never touch a repository.
type Service struct {
	repo  ScheduleConfigRepository
	now   func() time.Time
	newID func() string
}

// Option customizes a Service. The clock and ID source are injectable so tests
// can pin both.
type Option func(*Service)

// WithClock overrides the wall clock. One transition uses one time value in
// every row it writes.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithIDSource overrides identifier generation.
func WithIDSource(newID func() string) Option {
	return func(s *Service) { s.newID = newID }
}

// NewService builds a Service over a repository.
func NewService(repo ScheduleConfigRepository, opts ...Option) *Service {
	s := &Service{
		repo:  repo,
		now:   func() time.Time { return time.Now().UTC() },
		newID: func() string { return uuid.New().String() },
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateSchedule creates a schedule and its first revision in one transaction.
//
// After the upgrade the database holds no schedules, so this is the primary
// production flow rather than a migration edge case. There is no intermediate
// state in which a schedule exists without a revision: the whole of it is one
// repository operation.
//
// A concurrent create for the same team yields ErrScheduleExists.
func (s *Service) CreateSchedule(ctx context.Context, teamID string, config rotation.ScheduleConfiguration) (*ScheduleRevision, error) {
	if teamID == "" {
		return nil, fmt.Errorf("scheduleconfig: team id is required")
	}

	var created *ScheduleRevision
	err := s.repo.WithinTx(ctx, func(tx ScheduleConfigTx) error {
		// No schedule row exists yet, so there is nothing to lock and no tail
		// revision to stay ahead of: effective time is simply now.
		effectiveAt := NormalizeTimestamp(s.now().UTC())

		plan, err := rotation.PlanTransition(rotation.TransitionInput{
			Current:     nil,
			Desired:     config,
			EffectiveAt: effectiveAt,
		})
		if err != nil {
			return err
		}
		// A no-op needs a current configuration to be equal to; the planner
		// cannot report one here. Guard rather than assume.
		if plan.Noop {
			return fmt.Errorf("%w: initial transition reported as no-op", ErrInvariantViolation)
		}

		summary := plan.Change
		root := &ScheduleRoot{ID: s.newID(), TeamID: teamID}
		revision := &ScheduleRevision{
			ID:            s.newID(),
			ScheduleID:    root.ID,
			Version:       1,
			Snapshot:      plan.Snapshot,
			EffectiveFrom: effectiveAt,
			RecordedAt:    effectiveAt,
			ChangeSummary: &summary,
		}

		if err := tx.CreateInitialSchedule(ctx, root, revision); err != nil {
			return err
		}
		created = revision
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}
