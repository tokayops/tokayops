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

// previewBase is the state a preview is computed against, loaded once.
//
// A team with no schedule yet is not a special case here: it is a base with an
// empty schedule id, no current configuration, nobody on call and no overrides.
// Answering "does this schedule exist" once, at load time, is what keeps the
// computation below free of a flag threaded through five branches.
type previewBase struct {
	// scheduleID is empty when the team has no schedule. Nothing below queries
	// by it without checking, because there is nothing to find.
	scheduleID string

	root      scheduleconfig.ScheduleRoot
	current   *rotation.ScheduleRevisionSnapshot
	onCall    OnCall
	revisions []scheduleconfig.ScheduleRevision
	overrides []scheduleconfig.OverrideRevision
}

func (s *Service) preview(ctx context.Context, view scheduleconfig.ScheduleReadView, teamID string,
	desired rotation.ScheduleConfiguration, until *time.Time) (*PreviewResult, error) {

	evaluatedAt := scheduleconfig.NormalizeTimestamp(s.now().UTC())
	windowUntil := previewWindow(evaluatedAt, until)

	// Load first, validate second, in that order because Save also refuses a
	// schedule it cannot read before it looks at membership. A preview that
	// reported a membership problem where the save reports damage would send
	// the editor to fix the wrong thing.
	base, err := loadPreviewBase(ctx, view, teamID, evaluatedAt, windowUntil)
	if err != nil {
		return nil, err
	}
	if err := scheduleconfig.ValidateMembership(ctx, view, teamID,
		scheduleconfig.ConfigurationUserIDs(desired)); err != nil {
		return nil, err
	}

	plan, err := rotation.PlanTransition(rotation.TransitionInput{
		Current:     base.current,
		Desired:     desired,
		EffectiveAt: evaluatedAt,
	})
	if err != nil {
		return nil, err
	}

	// A no-op previews the chain as it stands. Splicing in a hypothetical
	// revision that is byte-identical to the tail would only invent a
	// boundary the save would never create.
	hypothetical := base.revisions
	if !plan.Noop {
		hypothetical = spliceHypothetical(base.revisions, base.scheduleID,
			base.root.ConfigVersion+1, plan.Snapshot, evaluatedAt)
	}

	rendered, err := Render(Input{
		Root:      base.root,
		Revisions: hypothetical,
		Overrides: base.overrides,
		From:      evaluatedAt,
		Until:     windowUntil,
	})
	if err != nil {
		return nil, err
	}

	res := &PreviewResult{
		EvaluatedAt:  evaluatedAt,
		BaseVersion:  base.root.ConfigVersion,
		OnCallBefore: base.onCall,
		Entries:      MergeAdjacent(rendered.Assignments),
		Warnings:     rendered.Warnings,
	}
	if plan.Noop {
		res.OnCallAfter = res.OnCallBefore
		return res, nil
	}

	after, err := s.onCallAfter(ctx, view, base, plan.Snapshot, evaluatedAt)
	if err != nil {
		return nil, err
	}
	res.OnCallAfter = after
	res.OnCallChanged = !sameDuty(res.OnCallBefore, res.OnCallAfter)
	return res, nil
}

// loadPreviewBase reads everything the preview computes against, in one pass
// over the snapshot.
//
// The root it returns is the one the renderer should see, not the one on disk:
// deleted_at is cleared because the save being previewed brings the schedule
// back, and a missing history horizon is set to the moment of evaluation so a
// hypothetical is not reported as incomplete history.
func loadPreviewBase(ctx context.Context, view scheduleconfig.ScheduleReadView, teamID string,
	evaluatedAt, windowUntil time.Time) (previewBase, error) {

	base := previewBase{
		root:   scheduleconfig.ScheduleRoot{TeamID: teamID, HistoryCompleteFrom: &evaluatedAt},
		onCall: OnCall{At: evaluatedAt},
	}

	root, err := view.GetScheduleRootByTeam(ctx, teamID)
	if errors.Is(err, scheduleconfig.ErrScheduleNotFound) {
		// Version 0, which is exactly what the create branch of Save requires.
		return base, nil
	}
	if err != nil {
		return previewBase{}, err
	}
	// An existing root reports its real version, deleted included: a recreate
	// is a save against that version, and reporting 0 would make the first
	// recreate fail optimistic concurrency every time.
	base.scheduleID = root.ID
	base.root = previewRoot(*root, evaluatedAt)

	// One read, two answers. The projection already loads the revision in force
	// to work out who is on duty, and asking for it again to fill `current` was
	// a second round trip for a row that was already in hand - and, worse, a
	// second chance for the two to disagree about which revision "in force"
	// meant.
	onCall, effective, err := onCallOfRoot(ctx, view, *root, evaluatedAt)
	if err != nil {
		return previewBase{}, err
	}
	base.onCall = onCall
	base.onCall.At = evaluatedAt

	// A deleted period carries a copy of the last valid snapshot so the column
	// stays decodable. It is not a configuration in force, so the planner is
	// given nothing - exactly as the recreate branch of Save. A nil revision is
	// the same story from the other side: the instant precedes this schedule's
	// history, so there is nothing in force to plan against.
	if effective != nil && effective.Kind == scheduleconfig.RevisionActive {
		base.current = &effective.Snapshot
	}

	if base.revisions, err = view.GetRevisionsInRange(ctx, root.ID, evaluatedAt, windowUntil); err != nil {
		return previewBase{}, err
	}
	// Existing overrides survive a save untouched, so the preview has to show
	// them: a window without them would promise the rotation is on duty during
	// a stand-in someone already arranged.
	if base.overrides, err = view.GetOverrideProjectionInRange(ctx, root.ID,
		&evaluatedAt, &windowUntil); err != nil {
		return previewBase{}, err
	}
	return base, nil
}

// onCallAfter projects the hypothetical revision.
//
// A team with no schedule has no overrides to fetch, and asking for them by an
// empty id would be a query that can only return nothing.
func (s *Service) onCallAfter(ctx context.Context, view scheduleconfig.ScheduleReadView,
	base previewBase, snapshot rotation.ScheduleRevisionSnapshot, at time.Time) (OnCall, error) {

	rev := scheduleconfig.ScheduleRevision{
		ID:            PreviewRevisionID,
		ScheduleID:    base.scheduleID,
		Version:       base.root.ConfigVersion + 1,
		Kind:          scheduleconfig.RevisionActive,
		Snapshot:      snapshot,
		EffectiveFrom: at,
	}
	if base.scheduleID == "" {
		return projectRevisionOnCall(rev, at, nil)
	}
	return onCallOfRevision(ctx, view, rev, at)
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
	// The one caller of RootInitialized that fills the gap in instead of
	// refusing: this root is hypothetical, so "no horizon" here means "no
	// schedule yet", not the skipped-reset damage it means everywhere else.
	if !scheduleconfig.RootInitialized(&root) {
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
