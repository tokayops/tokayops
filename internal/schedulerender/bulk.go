package schedulerender

import (
	"context"
	"errors"
	"time"

	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// ScheduleOnCall is who is on duty for one schedule, plus the few facts about
// the schedule its runtime consumers need to act on that.
//
// Timezone and SlackUsergroupID come from the snapshot of the revision in
// force, not from the schedule row. They are configuration, they are already
// loaded, and reading them anywhere else would be a second source of truth for
// them - which is exactly how the Slack syncer came to filter on a column the
// revision path never writes.
type ScheduleOnCall struct {
	ScheduleID       string
	TeamID           string
	Timezone         string
	SlackUsergroupID string

	// DeletedAt is set for a soft-deleted schedule. Such a schedule is
	// reported, with an empty OnCall: a consumer tracking duty changes has to
	// learn that this duty ended, and dropping the row here would leave it
	// believing the last group is still on call.
	DeletedAt *time.Time

	OnCall OnCall
}

// ProjectionFailureReason is why one schedule could not be projected. The set
// is closed so that a metric labelled with it stays bounded by construction,
// and so a consumer branches on a value rather than on prose.
type ProjectionFailureReason string

const (
	// FailureSnapshotDecode - the stored configuration no longer decodes.
	FailureSnapshotDecode ProjectionFailureReason = "snapshot_decode"

	// FailureRevisionMetadata - the audit metadata of a revision no longer
	// decodes. The configuration may be fine; the row still cannot be read.
	FailureRevisionMetadata ProjectionFailureReason = "revision_metadata_decode"

	// FailureRevisionGap - no revision is in force at an instant the chain was
	// supposed to cover.
	FailureRevisionGap ProjectionFailureReason = "revision_gap"

	// FailureRevisionOverlap - two revisions are in force at one instant.
	FailureRevisionOverlap ProjectionFailureReason = "revision_overlap"

	// FailureOverrideCollision - two live overrides of one layer cover one
	// instant in stored data.
	FailureOverrideCollision ProjectionFailureReason = "override_collision"

	// FailureRotation - the rotation math refused the stored configuration.
	FailureRotation ProjectionFailureReason = "rotation_error"
)

// ProjectionFailure is a schedule that could not be projected because of its
// own data. The renderer assigns the reason: doing it in each consumer would be
// the same classification written twice, and two copies of it would disagree.
type ProjectionFailure struct {
	ScheduleID string
	TeamID     string
	Reason     ProjectionFailureReason
	Err        error
}

// BulkOnCall is the answer for every schedule at one instant, with the ones
// that could not be answered listed separately.
//
// Failures are not folded into an error for the whole call. One corrupt
// schedule failing the tick is the defect this contract exists to remove: the
// notifier would then process none of the others, the syncer would sync none of
// the others, and every cache would stay a tick behind - a blast radius set by
// the unluckiest row in the table.
type BulkOnCall struct {
	Schedules []ScheduleOnCall
	Failures  []ProjectionFailure
}

// CurrentOnCallForAllNow projects every schedule at the service's own clock.
//
// The runtime calls this rather than the variant taking an instant, for the
// reason CurrentOnCallNow exists: a consumer reaching for time.Now() itself is
// a second clock, and it would silently ignore WithClock in tests while the
// preview honoured it.
func (s *Service) CurrentOnCallForAllNow(ctx context.Context) (BulkOnCall, error) {
	return s.CurrentOnCallForAll(ctx, s.now().UTC())
}

// CurrentOnCallForAll projects every schedule at one instant, inside one
// snapshot.
//
// One snapshot, not a loop over CurrentOnCall: each of those opens its own
// read-only transaction, so a tick over N schedules would open N of them, and
// the result would describe N different moments rather than one.
//
// The error return is reserved for failures of the call itself - the snapshot
// would not open, the list query failed, the driver broke. Once a statement
// errors, the transaction is aborted anyway and there is nothing left to
// continue inside it. Damage to a single schedule goes to Failures, and the
// default is closed: an error this package does not recognize is treated as
// infrastructure and fails the call, rather than quietly marking one schedule
// broken and letting a connection failure look like N corrupt rows.
func (s *Service) CurrentOnCallForAll(ctx context.Context, at time.Time) (BulkOnCall, error) {
	at = scheduleconfig.NormalizeTimestamp(at)

	var out BulkOnCall
	err := s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		roots, err := view.ListScheduleRoots(ctx)
		if err != nil {
			return err
		}

		out.Schedules = make([]ScheduleOnCall, 0, len(roots))
		for _, root := range roots {
			onCall, rev, err := onCallOfRoot(ctx, view, root, at)
			if err != nil {
				reason, ok := FailureReasonOf(err)
				if !ok {
					return err
				}
				out.Failures = append(out.Failures, ProjectionFailure{
					ScheduleID: root.ID,
					TeamID:     root.TeamID,
					Reason:     reason,
					Err:        err,
				})
				continue
			}

			sc := ScheduleOnCall{
				ScheduleID: root.ID,
				TeamID:     root.TeamID,
				DeletedAt:  root.DeletedAt,
				OnCall:     onCall,
			}
			// An instant before the schedule's history horizon has no revision
			// in force, so there is no snapshot to take these from. Every other
			// way of ending up without one is damage, and damage went to
			// Failures rather than here.
			if rev != nil {
				sc.Timezone = rev.Snapshot.Timezone
				sc.SlackUsergroupID = rev.Snapshot.SlackUsergroupID
			}
			out.Schedules = append(out.Schedules, sc)
		}
		return nil
	})
	if err != nil {
		return BulkOnCall{}, err
	}
	return out, nil
}

// FailureReasonOf classifies an error as damage to one schedule, or declines to.
//
// Classification is by sentinel, never by message: the reasons are a closed set
// and a text match would silently reclassify itself the next time someone
// rewords an error.
//
// It is exported because the distinction it draws is one every consumer of the
// projection has to make, not one only the bulk path cares about: damage to
// stored data will still be there on the next attempt, while a read that failed
// may not be. A consumer classifying for itself would be this switch written
// twice, and two copies of it would disagree - the same reason ProjectionFailure
// says the renderer assigns the reason.
//
// A NEW KIND OF DAMAGE MUST GET A CASE HERE, and this is no longer only about
// the label on a metric. The escalation builder branches on this answer: damage
// degrades a schedule step to a marker and lets the rest of the policy deliver,
// while everything else defers the whole escalation for the next tick to retry.
// Damage that arrives without a case is read as "the read failed", so the retry
// never succeeds and the alerts of that team reach nobody at all - not even the
// channel - for as long as the data stays broken.
func FailureReasonOf(err error) (ProjectionFailureReason, bool) {
	switch {
	case errors.Is(err, scheduleconfig.ErrSnapshotDecode):
		return FailureSnapshotDecode, true
	case errors.Is(err, scheduleconfig.ErrRevisionMetadataDecode):
		return FailureRevisionMetadata, true
	case errors.Is(err, ErrRevisionGap):
		return FailureRevisionGap, true
	// Both are damage to ONE schedule, so they belong here rather than failing
	// the tick: without these two cases a corrupt row would take the whole
	// projection down with it, which is the defect the bulk contract exists
	// to prevent.
	case errors.Is(err, scheduleconfig.ErrRevisionOverlap):
		return FailureRevisionOverlap, true
	case errors.Is(err, scheduleconfig.ErrOverrideCollision):
		return FailureOverrideCollision, true
	case errors.Is(err, ErrRotation):
		return FailureRotation, true
	default:
		return "", false
	}
}
