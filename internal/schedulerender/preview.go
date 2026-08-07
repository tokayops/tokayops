package schedulerender

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
)

// Preview window bounds. The default is two weeks because that is roughly what
// an editor shows; the cap matches the render endpoint, so a preview cannot
// make the renderer materialize years of daily slots.
const (
	PreviewDefaultWindow = 14 * 24 * time.Hour
	PreviewMaxWindow     = 90 * 24 * time.Hour
)

// PreviewRevisionID is the identifier the hypothetical revision carries. It is
// deliberately not a UUID: nothing was written, and a value that looks like a
// stored ID would invite a caller to fetch it.
const PreviewRevisionID = "preview"

// PreviewResult is what a save WOULD do, without doing it.
type PreviewResult struct {
	// EvaluatedAt is the instant the hypothetical revision would take effect.
	EvaluatedAt time.Time

	// BaseVersion is config_version as it stands, for the editor to send back
	// as expected_version.
	BaseVersion int64

	OnCallBefore  OnCall
	OnCallAfter   OnCall
	OnCallChanged bool

	Entries  []Shift
	Warnings []Warning
}

// Preview renders the schedule as it would look after a save.
//
// It lives on the read side and runs inside a read-only snapshot, so "this
// writes nothing" is a guarantee the database makes rather than a discipline
// the reviewer has to check. It shares its validation with Save through the
// scheduleconfig helpers: a preview that accepted what the save rejects would
// show the user a calculation they cannot commit.
//
// expected_version is deliberately not checked. The preview is advisory - the
// version it reports is what the subsequent save will collide on, and that
// save takes the lock that makes the check meaningful.
func (s *Service) Preview(ctx context.Context, teamID string,
	desired rotation.ScheduleConfiguration, until *time.Time) (*PreviewResult, error) {

	normalized, err := scheduleconfig.NormalizeConfiguration(desired)
	if err != nil {
		return nil, err
	}

	var out *PreviewResult
	err = s.repo.WithinSnapshot(ctx, func(view scheduleconfig.ScheduleReadView) error {
		res, err := s.preview(ctx, view, teamID, normalized, until)
		if err != nil {
			return err
		}
		out = res
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) preview(ctx context.Context, view scheduleconfig.ScheduleReadView, teamID string,
	desired rotation.ScheduleConfiguration, until *time.Time) (*PreviewResult, error) {

	evaluatedAt := scheduleconfig.NormalizeTimestamp(s.now().UTC())

	root, err := view.GetScheduleRootByTeam(ctx, teamID)
	absent := errors.Is(err, scheduleconfig.ErrScheduleNotFound)
	if err != nil && !absent {
		return nil, err
	}
	if !absent && scheduleconfig.IsLegacyRoot(root) {
		// The same refusal Save gives. Rendering a legacy schedule here would
		// show a plan that Save then rejects.
		return nil, scheduleconfig.ErrLegacySchedule
	}

	if err := scheduleconfig.ValidateMembership(ctx, view, teamID,
		scheduleconfig.ConfigurationUserIDs(desired)); err != nil {
		return nil, err
	}

	res := &PreviewResult{EvaluatedAt: evaluatedAt}
	var (
		current   *rotation.ScheduleRevisionSnapshot
		revisions []scheduleconfig.ScheduleRevision
	)
	windowUntil := previewWindow(evaluatedAt, until)

	if absent {
		// A schedule that does not exist yet has version 0, and the create
		// branch of Save requires exactly that.
		root = &scheduleconfig.ScheduleRoot{TeamID: teamID, HistoryCompleteFrom: &evaluatedAt}
	} else {
		// Every existing root reports its real version, deleted included: a
		// recreate is a save against that version, and reporting 0 would make
		// the first recreate fail optimistic concurrency every time.
		res.BaseVersion = root.ConfigVersion

		effective, err := view.GetEffectiveRevision(ctx, root.ID, evaluatedAt)
		if err != nil && !errors.Is(err, scheduleconfig.ErrRevisionNotFound) {
			return nil, err
		}
		// A deleted period carries a copy of the last valid snapshot so the
		// column stays decodable. It is not a configuration in force, so the
		// planner is given nothing - exactly as the recreate branch of Save.
		if err == nil && effective.Kind == scheduleconfig.RevisionActive {
			current = &effective.Snapshot
		}
		if res.OnCallBefore, err = onCallWithin(ctx, view, root.ID, evaluatedAt); err != nil {
			return nil, err
		}
		if revisions, err = view.GetRevisionsInRange(ctx, root.ID, evaluatedAt, windowUntil); err != nil {
			return nil, err
		}
	}
	res.OnCallBefore.At = evaluatedAt

	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Current:     current,
		Desired:     desired,
		EffectiveAt: evaluatedAt,
	})
	if err != nil {
		return nil, err
	}

	// A no-op previews the chain as it stands. Splicing in a hypothetical
	// revision that is byte-identical to the tail would only invent a
	// boundary the save would never create.
	hypothetical := revisions
	if !plan.Noop {
		hypothetical = spliceHypothetical(revisions, root.ID, root.ConfigVersion+1, plan.Snapshot, evaluatedAt)
	}

	// Existing overrides survive a save untouched, so the preview has to show
	// them: a rendered window without them would promise the rotation is on
	// duty during a stand-in someone already arranged.
	var overrides []scheduleconfig.OverrideRevision
	if !absent {
		if overrides, err = view.GetOverrideProjectionInRange(ctx, root.ID,
			&evaluatedAt, &windowUntil, nil); err != nil {
			return nil, err
		}
	}

	rendered, err := Render(Input{
		Root:      previewRoot(*root, evaluatedAt),
		Revisions: hypothetical,
		Overrides: overrides,
		From:      evaluatedAt,
		Until:     windowUntil,
	})
	if err != nil {
		return nil, err
	}
	res.Entries = MergeAdjacent(rendered.Assignments)
	res.Warnings = rendered.Warnings

	if plan.Noop {
		res.OnCallAfter = res.OnCallBefore
		res.OnCallChanged = false
		return res, nil
	}

	after, err := onCallOfRevision(ctx, view, scheduleconfig.ScheduleRevision{
		ID:            PreviewRevisionID,
		ScheduleID:    root.ID,
		Version:       root.ConfigVersion + 1,
		Kind:          scheduleconfig.RevisionActive,
		Snapshot:      plan.Snapshot,
		EffectiveFrom: evaluatedAt,
	}, evaluatedAt)
	if err != nil {
		return nil, err
	}
	res.OnCallAfter = after
	res.OnCallChanged = !sameDuty(res.OnCallBefore, res.OnCallAfter)
	return res, nil
}

// previewWindow clamps the requested end of the entries window. Unlike the
// render endpoint, which rejects an over-long range, the preview normalizes:
// it is advisory, and the editor asking for more than it can be shown is not
// an error worth failing the whole calculation over.
func previewWindow(evaluatedAt time.Time, until *time.Time) time.Time {
	if until == nil {
		return evaluatedAt.Add(PreviewDefaultWindow)
	}
	requested := scheduleconfig.NormalizeTimestamp(*until)
	if !requested.After(evaluatedAt) {
		return evaluatedAt.Add(PreviewDefaultWindow)
	}
	if max := evaluatedAt.Add(PreviewMaxWindow); requested.After(max) {
		return max
	}
	return requested
}

// previewRoot presents the root as the hypothetical world would have it: not
// deleted, since the save being previewed recreates it, and with a known
// history horizon so a preview of a schedule that does not exist yet is not
// reported as incomplete history.
func previewRoot(root scheduleconfig.ScheduleRoot, evaluatedAt time.Time) scheduleconfig.ScheduleRoot {
	root.DeletedAt = nil
	if root.HistoryCompleteFrom == nil {
		at := evaluatedAt
		root.HistoryCompleteFrom = &at
	}
	return root
}

// spliceHypothetical closes the open-ended revision at evaluatedAt and appends
// the revision the plan would produce.
//
// The input is copied: these revisions came out of a snapshot the caller may
// still be reading, and a preview must not edit the history it is previewing
// against even in memory.
func spliceHypothetical(revisions []scheduleconfig.ScheduleRevision, scheduleID string,
	version int64, snapshot rotation.ScheduleRevisionSnapshot, evaluatedAt time.Time) []scheduleconfig.ScheduleRevision {

	out := make([]scheduleconfig.ScheduleRevision, 0, len(revisions)+1)
	for _, rev := range revisions {
		if rev.EffectiveTo == nil {
			at := evaluatedAt
			rev.EffectiveTo = &at
		}
		out = append(out, rev)
	}
	return append(out, scheduleconfig.ScheduleRevision{
		ID:            PreviewRevisionID,
		ScheduleID:    scheduleID,
		Version:       version,
		Kind:          scheduleconfig.RevisionActive,
		Snapshot:      snapshot,
		EffectiveFrom: evaluatedAt,
		RecordedAt:    evaluatedAt,
	})
}

// sameDuty compares two projections by who is on duty and why.
//
// Provenance is excluded on purpose: a save always produces a new revision, so
// comparing revision IDs would report "changed" for every save, including the
// ones that only move a Slack usergroup. Membership is compared as a set - the
// order inside a group is not something the rotation attaches meaning to.
func sameDuty(a, b OnCall) bool {
	return sameLayerDuty(a.L1, b.L1) && sameLayerDuty(a.L2, b.L2)
}

func sameLayerDuty(a, b *LayerOnCall) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Source != b.Source {
		return false
	}
	return equalUserSets(a.UserIDs, b.UserIDs)
}

func equalUserSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
