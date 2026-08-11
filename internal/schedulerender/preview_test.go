package schedulerender_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/rotation"
	"github.com/tokayops/tokayops/internal/scheduleconfig"
	"github.com/tokayops/tokayops/internal/scheduleconfig/fakes"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

const (
	pvGroupA = "7c0a1e2c-2222-4a3b-8c4d-000000000001"
	pvGroupB = "7c0a1e2c-2222-4a3b-8c4d-000000000002"
	pvGroupC = "7c0a1e2c-2222-4a3b-8c4d-000000000003"
)

var previewNow = time.Date(2026, 5, 4, 8, 30, 0, 0, time.UTC)

func previewConfig(groups ...rotation.RotationGroup) rotation.ScheduleConfiguration {
	monday := 1
	weekly := rotation.RotationPolicy{
		SchemaVersion: rotation.PolicySchemaVersion,
		Cadence:       model.RotationWeekly,
		HandoffTime:   "11:00",
		HandoffDay:    &monday,
	}
	return rotation.ScheduleConfiguration{
		Timezone: "UTC",
		L1:       rotation.LayerConfiguration{Enabled: true, Policy: weekly, Groups: groups},
		L2:       rotation.LayerConfiguration{Enabled: false, Policy: weekly},

		L2EscalationTimeoutMins: 5,
	}
}

func pvGroup(id string, members ...string) rotation.RotationGroup {
	return rotation.RotationGroup{ID: id, Members: members}
}

type previewFixture struct {
	svc    *schedulerender.Service
	config *scheduleconfig.Service
	repo   *fakes.ScheduleConfigRepo
	now    time.Time
}

func newPreviewFixture(t *testing.T) *previewFixture {
	t.Helper()
	repo := fakes.NewScheduleConfigRepo()
	repo.SetTeamMembers("devops", "alice", "bob", "carol")

	f := &previewFixture{repo: repo, now: previewNow}
	clock := func() time.Time { return f.now }
	f.svc = schedulerender.New(repo, schedulerender.WithClock(clock))
	f.config = scheduleconfig.NewService(repo,
		scheduleconfig.WithClock(clock),
		scheduleconfig.WithLogger(func(string, ...any) {}))
	return f
}

func (f *previewFixture) save(t *testing.T, version int64, cfg rotation.ScheduleConfiguration) *scheduleconfig.SaveResult {
	t.Helper()
	res, err := f.config.Save(context.Background(), "devops", scheduleconfig.SaveCommand{
		ExpectedVersion: version,
		Desired:         cfg,
		ActorID:         "alice",
	})
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	return res
}

func (f *previewFixture) preview(t *testing.T, cfg rotation.ScheduleConfiguration, until *time.Time) *schedulerender.PreviewResult {
	t.Helper()
	res, err := f.svc.Preview(context.Background(), "devops", cfg, until)
	if err != nil {
		t.Fatalf("Preview: %v", err)
	}
	return res
}

// The read-only snapshot makes "writes nothing" a property of the transaction
// rather than of the code inside it, but the fake can still prove the effect.
func TestPreviewWritesNothing(t *testing.T) {
	f := newPreviewFixture(t)
	created := f.save(t, 0, previewConfig(pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob")))

	before := len(f.repo.Revisions(created.Revision.ScheduleID))
	f.preview(t, previewConfig(pvGroup(pvGroupC, "carol")), nil)

	if got := len(f.repo.Revisions(created.Revision.ScheduleID)); got != before {
		t.Fatalf("preview wrote a revision: %d, want %d", got, before)
	}
	root, _ := f.repo.RootByTeam("devops")
	if root.ConfigVersion != created.Version {
		t.Fatalf("preview moved the config version to %d", root.ConfigVersion)
	}
}

func TestPreviewNoopReportsOnCallUnchanged(t *testing.T) {
	f := newPreviewFixture(t)
	cfg := previewConfig(pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob"))
	f.save(t, 0, cfg)

	f.now = f.now.Add(time.Hour)
	res := f.preview(t, cfg, nil)

	if res.OnCallChanged {
		t.Fatal("previewing the configuration already in force must report no change")
	}
	if res.OnCallBefore.L1 == nil || res.OnCallAfter.L1 == nil {
		t.Fatalf("both projections must name who is on duty: %+v / %+v", res.OnCallBefore, res.OnCallAfter)
	}
	if res.OnCallBefore.L1.GroupID != res.OnCallAfter.L1.GroupID {
		t.Fatalf("a no-op changed the group on duty: %s -> %s",
			res.OnCallBefore.L1.GroupID, res.OnCallAfter.L1.GroupID)
	}
	if len(res.Entries) == 0 {
		t.Fatal("a no-op preview must still render the current chain")
	}
}

// The original bug, seen from the preview: adding someone to the group on
// duty changes who is on call without restarting the rotation.
func TestPreviewShowsBugScenarioChange(t *testing.T) {
	f := newPreviewFixture(t)
	f.save(t, 0, previewConfig(
		pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob"), pvGroup(pvGroupC, "carol")))

	// Ten minutes later, still inside the same weekly slot.
	f.now = f.now.Add(10 * time.Minute)
	res := f.preview(t, previewConfig(
		pvGroup(pvGroupA, "alice", "carol"), pvGroup(pvGroupB, "bob"), pvGroup(pvGroupC, "carol")), nil)

	if !res.OnCallChanged {
		t.Fatal("adding someone to the group on duty changes who is on call")
	}
	if res.OnCallAfter.L1 == nil || len(res.OnCallAfter.L1.UserIDs) != 2 {
		t.Fatalf("on_call_after should name both members, got %+v", res.OnCallAfter.L1)
	}
	if res.OnCallBefore.L1.GroupID != res.OnCallAfter.L1.GroupID {
		t.Fatalf("the rotation restarted: %s -> %s",
			res.OnCallBefore.L1.GroupID, res.OnCallAfter.L1.GroupID)
	}
}

// The preview must refuse exactly what the save refuses, or the editor shows
// a calculation it then cannot commit.
func TestPreviewValidationMatchesSave(t *testing.T) {
	t.Run("membership", func(t *testing.T) {
		f := newPreviewFixture(t)
		_, err := f.svc.Preview(context.Background(), "devops",
			previewConfig(pvGroup(pvGroupA, "mallory")), nil)

		var notMember *scheduleconfig.UserNotTeamMemberError
		if !errors.As(err, &notMember) {
			t.Fatalf("error = %v, want a membership rejection", err)
		}
	})

	t.Run("shape", func(t *testing.T) {
		f := newPreviewFixture(t)
		cfg := previewConfig(pvGroup(pvGroupA, "alice"))
		cfg.Timezone = "Mars/Olympus"

		_, err := f.svc.Preview(context.Background(), "devops", cfg, nil)
		if !errors.Is(err, scheduleconfig.ErrValidation) {
			t.Fatalf("error = %v, want a validation rejection", err)
		}
	})

	// A root with no history horizon is damage, not a schedule the preview can
	// reason about: showing a plan for it would show a plan Save then refuses.
	t.Run("root without history", func(t *testing.T) {
		f := newPreviewFixture(t)
		f.repo.SeedRootWithoutHistory("legacy-1", "devops")

		_, err := f.svc.Preview(context.Background(), "devops",
			previewConfig(pvGroup(pvGroupA, "alice")), nil)
		if !errors.Is(err, schedulerender.ErrHistoryMarkerMissing) {
			t.Fatalf("error = %v, want ErrHistoryMarkerMissing", err)
		}
	})
}

func TestPreviewOnAbsentSchedule(t *testing.T) {
	f := newPreviewFixture(t)
	res := f.preview(t, previewConfig(pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob")), nil)

	if res.BaseVersion != 0 {
		t.Fatalf("base_version = %d, want 0 for a schedule that does not exist", res.BaseVersion)
	}
	if res.OnCallBefore.L1 != nil || res.OnCallBefore.L2 != nil {
		t.Fatalf("nobody can be on duty before the schedule exists: %+v", res.OnCallBefore)
	}
	if res.OnCallAfter.L1 == nil {
		t.Fatal("on_call_after must name who the new rotation puts on duty")
	}
	if !res.OnCallChanged {
		t.Fatal("creating a rotation where there was none is a change")
	}
	if len(res.Entries) == 0 {
		t.Fatal("preview rendered no entries")
	}
}

// A deleted schedule reports its real version. Reporting 0 - the value that
// means "does not exist" - would make the first recreate fail optimistic
// concurrency every time.
func TestPreviewOnDeletedScheduleKeepsConfigVersion(t *testing.T) {
	f := newPreviewFixture(t)
	f.save(t, 0, previewConfig(pvGroup(pvGroupA, "alice")))
	f.now = f.now.Add(time.Hour)
	if err := f.config.Delete(context.Background(), "devops",
		scheduleconfig.DeleteCommand{ExpectedVersion: 1, ActorID: "alice"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	f.now = f.now.Add(time.Hour)
	res := f.preview(t, previewConfig(pvGroup(pvGroupB, "bob")), nil)

	if res.BaseVersion != 2 {
		t.Fatalf("base_version = %d, want the deleted schedule's version 2", res.BaseVersion)
	}
	if res.OnCallBefore.L1 != nil {
		t.Fatal("a deleted schedule has nobody on duty")
	}
	if res.OnCallAfter.L1 == nil || res.OnCallAfter.L1.GroupID != pvGroupB {
		t.Fatalf("the recreate must start the rotation at its first group, got %+v", res.OnCallAfter.L1)
	}
}

func TestPreviewWindowDefaultsAndCaps(t *testing.T) {
	f := newPreviewFixture(t)
	f.save(t, 0, previewConfig(pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob")))
	cfg := previewConfig(pvGroup(pvGroupB, "bob"), pvGroup(pvGroupA, "alice"))

	f.now = f.now.Add(10 * time.Minute)
	def := f.preview(t, cfg, nil)
	last := def.Entries[len(def.Entries)-1]
	if last.End.After(def.EvaluatedAt.Add(schedulerender.PreviewDefaultWindow)) {
		t.Fatalf("default window reached %v, past %v",
			last.End, def.EvaluatedAt.Add(schedulerender.PreviewDefaultWindow))
	}

	// An over-long request is normalized rather than rejected: the preview is
	// advisory, and failing the whole calculation over a window would be a
	// worse answer than a shorter one.
	huge := f.now.Add(365 * 24 * time.Hour)
	capped := f.preview(t, cfg, &huge)
	last = capped.Entries[len(capped.Entries)-1]
	if last.End.After(capped.EvaluatedAt.Add(schedulerender.PreviewMaxWindow)) {
		t.Fatalf("window was not capped: last entry ends %v", last.End)
	}
	if len(capped.Entries) <= len(def.Entries) {
		t.Fatalf("the capped window should still be longer than the default: %d vs %d",
			len(capped.Entries), len(def.Entries))
	}
}

func TestPreviewEvaluatedAtNormalized(t *testing.T) {
	f := newPreviewFixture(t)
	f.now = time.Date(2026, 5, 4, 8, 30, 0, 123456789, time.UTC)

	res := f.preview(t, previewConfig(pvGroup(pvGroupA, "alice")), nil)
	if res.EvaluatedAt != scheduleconfig.NormalizeTimestamp(f.now) {
		t.Fatalf("evaluated_at = %v, want it truncated to database resolution", res.EvaluatedAt)
	}
}

// An override in force survives a save untouched, so the preview has to show
// it: a window without it would promise the rotation is on duty during a
// stand-in somebody already arranged.
func TestPreviewKeepsExistingOverrides(t *testing.T) {
	f := newPreviewFixture(t)
	f.save(t, 0, previewConfig(pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob")))
	if _, err := f.config.CreateOverride(context.Background(), "devops", scheduleconfig.OverrideCommand{
		UserID:    "carol",
		ValidFrom: f.now.Add(24 * time.Hour),
		ValidTo:   f.now.Add(36 * time.Hour),
		ActorID:   "alice",
	}); err != nil {
		t.Fatalf("CreateOverride: %v", err)
	}

	f.now = f.now.Add(10 * time.Minute)
	res := f.preview(t, previewConfig(pvGroup(pvGroupB, "bob"), pvGroup(pvGroupA, "alice")), nil)

	var sawOverride bool
	for _, entry := range res.Entries {
		if entry.Source == schedulerender.SourceOverride {
			sawOverride = true
			if len(entry.UserIDs) != 1 || entry.UserIDs[0] != "carol" {
				t.Fatalf("override entry names %v, want carol", entry.UserIDs)
			}
		}
	}
	if !sawOverride {
		t.Fatal("the previewed window dropped an override that is in force")
	}
}

// TestPreviewReadsTheEffectiveRevisionOnce pins a count rather than a shape,
// because the defect it guards against does not change any answer.
//
// The preview needs the revision in force twice over: once to say who is on
// duty now, once as the state the planner edits. Those used to be two reads of
// the same row in the same snapshot. Nothing was WRONG with the second read -
// that is exactly why it survived review - it was a round trip nobody needed
// and a second place where "in force" could come to mean something else.
//
// A count is the only thing that fails when it comes back.
func TestPreviewReadsTheEffectiveRevisionOnce(t *testing.T) {
	f := newPreviewFixture(t)
	f.save(t, 0, previewConfig(pvGroup(pvGroupA, "alice"), pvGroup(pvGroupB, "bob")))

	f.repo.Calls = nil
	if _, err := f.svc.Preview(context.Background(), "devops",
		previewConfig(pvGroup(pvGroupA, "alice", "carol"), pvGroup(pvGroupB, "bob")), nil); err != nil {
		t.Fatalf("Preview: %v", err)
	}

	var reads int
	for _, call := range f.repo.Calls {
		if call == "GetEffectiveRevision" {
			reads++
		}
	}
	if reads != 1 {
		t.Fatalf("GetEffectiveRevision called %d times, want 1: %v", reads, f.repo.Calls)
	}
}
