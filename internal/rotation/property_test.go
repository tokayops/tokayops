package rotation

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// Fixed PCG seeds: every property run is deterministic. On failure both
// seeds and the iteration number are printed so the exact input can be
// reproduced by re-running with the same constants.
const (
	propSeed1 uint64 = 20260807
	propSeed2 uint64 = 424242
)

func newPropRand() *rand.Rand {
	return rand.New(rand.NewPCG(propSeed1, propSeed2))
}

func propFatalf(t *testing.T, iter int, format string, args ...any) {
	t.Helper()
	t.Fatalf("seeds=(%d,%d) iter=%d: %s", propSeed1, propSeed2, iter, fmt.Sprintf(format, args...))
}

// Curated timezones: plain UTC, EU and US DST, a +05:45 offset, a 30-minute
// DST shift, and southern-hemisphere DST with midnight transitions.
var propTimezones = []string{
	"UTC",
	"Europe/Berlin",
	"America/New_York",
	"Asia/Kathmandu",
	"Australia/Lord_Howe",
	"America/Santiago",
	"Pacific/Apia", // skipped the whole 2011-12-30 date; +13/+14 DST
}

func randTimezone(r *rand.Rand) string {
	return propTimezones[r.IntN(len(propTimezones))]
}

func randPolicy(r *rand.Rand) RotationPolicy {
	hh := r.IntN(24)
	mm := []int{0, 15, 30, 45, r.IntN(60)}[r.IntN(5)]
	handoff := fmt.Sprintf("%02d:%02d", hh, mm)
	if r.IntN(2) == 0 {
		return dailyPolicy(handoff)
	}
	return weeklyPolicy(handoff, r.IntN(7))
}

func randUUID(r *rand.Rand) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		r.Uint64()&0xffffffff, r.Uint64()&0xffff, r.Uint64()&0xffff,
		r.Uint64()&0xffff, r.Uint64()&0xffffffffffff)
}

func randInstant(r *rand.Rand, fromYear, toYear int) time.Time {
	from := time.Date(fromYear, time.January, 1, 0, 0, 0, 0, time.UTC).Unix()
	to := time.Date(toYear, time.December, 31, 0, 0, 0, 0, time.UTC).Unix()
	return time.Unix(from+r.Int64N(to-from), 0).UTC()
}

func randL1Groups(r *rand.Rand, iter int) []RotationGroup {
	n := 1 + r.IntN(4)
	out := make([]RotationGroup, n)
	for i := range out {
		members := make([]string, 1+r.IntN(3))
		for j := range members {
			members[j] = fmt.Sprintf("u%d_%d_%d", iter, i, j)
		}
		out[i] = RotationGroup{ID: randUUID(r), Members: members}
	}
	return out
}

// randActiveSnapshot builds a random VALID snapshot with an active L1 and a
// randomly enabled L2; anchors are genuine grid boundaries.
func randActiveSnapshot(t *testing.T, r *rand.Rand, iter int) ScheduleRevisionSnapshot {
	t.Helper()
	s := ScheduleRevisionSnapshot{
		SchemaVersion:           SnapshotSchemaVersion,
		Timezone:                randTimezone(r),
		SlackUsergroupID:        "S0123",
		L2EscalationTimeoutMins: 1 + r.IntN(1440),
	}
	s.L1 = RotationLayerSnapshot{Enabled: true, Policy: randPolicy(r), Groups: randL1Groups(r, iter)}
	g1, err := NewGrid(s.Timezone, s.L1.Policy)
	if err != nil {
		propFatalf(t, iter, "NewGrid l1: %v", err)
	}
	anchor1 := g1.SlotContaining(randInstant(r, 2024, 2027)).Start
	s.L1.PhaseAnchorSlotStart = &anchor1
	s.L1.StartPosition = intp(r.IntN(len(s.L1.Groups)))

	if r.IntN(2) == 0 {
		users := make([]string, 1+r.IntN(3))
		for i := range users {
			users[i] = fmt.Sprintf("x%d_%d", iter, i)
		}
		s.L2 = RotationLayerSnapshot{Enabled: true, Policy: randPolicy(r), Groups: L2GroupsFromUserIDs(users)}
		g2, err := NewGrid(s.Timezone, s.L2.Policy)
		if err != nil {
			propFatalf(t, iter, "NewGrid l2: %v", err)
		}
		anchor2 := g2.SlotContaining(randInstant(r, 2024, 2027)).Start
		s.L2.PhaseAnchorSlotStart = &anchor2
		s.L2.StartPosition = intp(r.IntN(len(s.L2.Groups)))
	} else {
		s.L2 = RotationLayerSnapshot{Enabled: false, Policy: weeklyPolicy("11:00", 1)}
	}
	if err := s.Validate(); err != nil {
		propFatalf(t, iter, "generated snapshot invalid: %v\n%+v", err, s)
	}
	return s
}

// projEntry is the rotation-core projection of one slot. This helper is
// test-only on purpose and must not grow into an exported renderer.
type projEntry struct {
	SlotStart time.Time
	SlotEnd   time.Time
	GroupID   string
	Members   []string
}

func renderProjection(t *testing.T, iter int, tz string, layer RotationLayerSnapshot, from, until time.Time) []projEntry {
	t.Helper()
	if !layer.Enabled || len(layer.Groups) == 0 {
		return nil
	}
	g, err := NewGrid(tz, layer.Policy)
	if err != nil {
		propFatalf(t, iter, "NewGrid: %v", err)
	}
	var out []projEntry
	for _, slot := range g.SlotsOverlapping(from, until) {
		pos, _, err := PositionAt(g, layer, slot.Start)
		if err != nil {
			propFatalf(t, iter, "PositionAt: %v", err)
		}
		gr := layer.Groups[pos]
		out = append(out, projEntry{
			SlotStart: slot.Start, SlotEnd: slot.End,
			GroupID: gr.ID, Members: append([]string(nil), gr.Members...),
		})
	}
	return out
}

func projectionsEqual(a, b []projEntry, compareMembers bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].SlotStart.Equal(b[i].SlotStart) || !a[i].SlotEnd.Equal(b[i].SlotEnd) || a[i].GroupID != b[i].GroupID {
			return false
		}
		if compareMembers {
			if len(a[i].Members) != len(b[i].Members) {
				return false
			}
			for j := range a[i].Members {
				if a[i].Members[j] != b[i].Members[j] {
					return false
				}
			}
		}
	}
	return true
}

// Property 1: a metadata-only Save (slack usergroup, escalation timeout)
// carries every active layer and renders the IDENTICAL full projection,
// members included, over [T, T+8w].
func TestProp_MetadataOnlySave_IdenticalProjection(t *testing.T) {
	r := newPropRand()
	for iter := 0; iter < 200; iter++ {
		snap := randActiveSnapshot(t, r, iter)
		effectiveAt := randInstant(r, 2025, 2027)

		desired := ConfigurationFromSnapshot(snap)
		desired.SlackUsergroupID = "S-metadata-edit"
		desired.L2EscalationTimeoutMins = 1 + (desired.L2EscalationTimeoutMins % 1440)
		if desired.L2EscalationTimeoutMins == snap.L2EscalationTimeoutMins {
			desired.L2EscalationTimeoutMins = 1 + desired.L2EscalationTimeoutMins%1439
		}

		plan, err := PlanTransition(TransitionInput{Current: &snap, Desired: desired, EffectiveAt: effectiveAt})
		if err != nil {
			propFatalf(t, iter, "PlanTransition: %v", err)
		}
		if plan.Noop {
			propFatalf(t, iter, "metadata edit classified as no-op")
		}
		if plan.L1.PhaseAction != PhaseActionCarry {
			propFatalf(t, iter, "L1 action = %s, want carry", plan.L1.PhaseAction)
		}
		if snap.L2.Enabled && plan.L2.PhaseAction != PhaseActionCarry {
			propFatalf(t, iter, "L2 action = %s, want carry", plan.L2.PhaseAction)
		}
		if !plan.Snapshot.L1.PhaseAnchorSlotStart.Equal(*snap.L1.PhaseAnchorSlotStart) ||
			*plan.Snapshot.L1.StartPosition != *snap.L1.StartPosition {
			propFatalf(t, iter, "L1 phase pair not copied verbatim")
		}

		until := effectiveAt.Add(8 * 7 * 24 * time.Hour)
		for _, layer := range []string{"l1", "l2"} {
			var before, after RotationLayerSnapshot
			if layer == "l1" {
				before, after = snap.L1, plan.Snapshot.L1
			} else {
				before, after = snap.L2, plan.Snapshot.L2
			}
			pb := renderProjection(t, iter, snap.Timezone, before, effectiveAt, until)
			pa := renderProjection(t, iter, plan.Snapshot.Timezone, after, effectiveAt, until)
			if !projectionsEqual(pb, pa, true) {
				propFatalf(t, iter, "%s projection changed after metadata-only save\nsnapshot: %+v", layer, snap)
			}
		}
	}
}

// Property 2: a composition of N carry transitions (membership edits
// included) never drifts the phase: the pair stays byte-equal and the phase
// projection (slot, group ID) is unchanged. Members legitimately change, so
// they are excluded here; property 1 covers full equality.
func TestProp_CarryComposition_NoPhaseDrift(t *testing.T) {
	r := newPropRand()
	for iter := 0; iter < 200; iter++ {
		initial := randActiveSnapshot(t, r, iter)
		cur := initial
		at := randInstant(r, 2025, 2026)

		for k := 0; k < 4; k++ {
			desired := ConfigurationFromSnapshot(cur)
			// Membership-only edits: IDs stay, members change; group 0
			// always mutated so the edit is never a no-op.
			for i := range desired.L1.Groups {
				if i == 0 || r.IntN(2) == 0 {
					desired.L1.Groups[i].Members = append(desired.L1.Groups[i].Members,
						fmt.Sprintf("m%d_%d_%d", iter, k, i))
				}
			}
			plan, err := PlanTransition(TransitionInput{Current: &cur, Desired: desired, EffectiveAt: at})
			if err != nil {
				propFatalf(t, iter, "step %d: PlanTransition: %v", k, err)
			}
			if plan.L1.PhaseAction != PhaseActionCarry {
				propFatalf(t, iter, "step %d: action = %s, want carry", k, plan.L1.PhaseAction)
			}
			cur = plan.Snapshot
			at = at.Add(time.Duration(1+r.IntN(72)) * time.Hour)
		}

		if !cur.L1.PhaseAnchorSlotStart.Equal(*initial.L1.PhaseAnchorSlotStart) ||
			*cur.L1.StartPosition != *initial.L1.StartPosition {
			propFatalf(t, iter, "phase pair drifted after carry composition:\ninitial (%v, %d)\nfinal (%v, %d)",
				initial.L1.PhaseAnchorSlotStart, *initial.L1.StartPosition,
				cur.L1.PhaseAnchorSlotStart, *cur.L1.StartPosition)
		}
		probeFrom := randInstant(r, 2025, 2026)
		pi := renderProjection(t, iter, initial.Timezone, initial.L1, probeFrom, probeFrom.Add(21*24*time.Hour))
		pf := renderProjection(t, iter, cur.Timezone, cur.L1, probeFrom, probeFrom.Add(21*24*time.Hour))
		if !projectionsEqual(pi, pf, false) {
			propFatalf(t, iter, "phase projection changed after carry composition")
		}
	}
}

// Property 3: PositionAt with a multi-year carry anchor agrees with a naive
// NextBoundary-stepping reference across every DST and historical rule
// change (including whole skipped dates) in between.
func TestProp_MultiYearCarryAnchor_DST(t *testing.T) {
	r := newPropRand()
	for iter := 0; iter < 200; iter++ {
		tz := randTimezone(r)
		policy := randPolicy(r)
		g, err := NewGrid(tz, policy)
		if err != nil {
			propFatalf(t, iter, "NewGrid: %v", err)
		}
		anchor := g.SlotContaining(randInstant(r, 2020, 2021)).Start
		probe := anchor.AddDate(0, 0, 800+r.IntN(1500)).Add(time.Duration(r.IntN(24)) * time.Hour)
		target := g.SlotContaining(probe).Start

		n, err := g.SlotsBetween(anchor, target)
		if err != nil {
			propFatalf(t, iter, "SlotsBetween: %v", err)
		}
		count := 0
		for b := anchor; b.Before(target); count++ {
			b = g.NextBoundary(b)
			if count > 4000 {
				propFatalf(t, iter, "naive stepping ran away (tz=%s policy=%+v)", tz, policy)
			}
			if !b.Before(target) && !b.Equal(target) {
				propFatalf(t, iter, "naive stepping overshot target: %v vs %v (tz=%s)", b, target, tz)
			}
		}
		if n != count {
			propFatalf(t, iter, "SlotsBetween=%d, naive=%d (tz=%s policy=%+v anchor=%v target=%v)",
				n, count, tz, policy, anchor, target)
		}
		back, err := g.SlotsBetween(target, anchor)
		if err != nil || back != -n {
			propFatalf(t, iter, "antisymmetry broken: %d vs %d (err=%v)", n, back, err)
		}

		// The plan-level check: PositionAt against the stepping reference.
		nGroups := 1 + r.IntN(4)
		sp := r.IntN(nGroups)
		layer := RotationLayerSnapshot{
			Enabled:              true,
			Policy:               policy,
			Groups:               make([]RotationGroup, nGroups),
			PhaseAnchorSlotStart: &anchor,
			StartPosition:        &sp,
		}
		for i := range layer.Groups {
			layer.Groups[i] = RotationGroup{ID: gid[i], Members: []string{gid[i]}}
		}
		pos, slot, err := PositionAt(g, layer, probe)
		if err != nil {
			propFatalf(t, iter, "PositionAt: %v", err)
		}
		if want := floorMod(sp+count, nGroups); pos != want {
			propFatalf(t, iter, "PositionAt=%d, reference=%d (sp=%d count=%d n=%d tz=%s)",
				pos, want, sp, count, nGroups, tz)
		}
		if !slot.Start.Equal(target) {
			propFatalf(t, iter, "PositionAt slot %v != target %v", slot.Start, target)
		}
	}
}

// Property 4: SlotsOverlapping tiles [from, until) with contiguous slots
// consistent with SlotContaining and NextBoundary.
func TestProp_SlotsOverlapping_TilesRangeNoGaps(t *testing.T) {
	r := newPropRand()
	for iter := 0; iter < 200; iter++ {
		tz := randTimezone(r)
		policy := randPolicy(r)
		g, err := NewGrid(tz, policy)
		if err != nil {
			propFatalf(t, iter, "NewGrid: %v", err)
		}
		from := randInstant(r, 2024, 2027)
		until := from.Add(time.Duration(1+r.IntN(60*24)) * time.Hour)

		slots := g.SlotsOverlapping(from, until)
		if len(slots) == 0 {
			propFatalf(t, iter, "no slots for a non-empty range")
		}
		if slots[0].Start.After(from) || !from.Before(slots[0].End) {
			propFatalf(t, iter, "first slot [%v,%v) does not contain from=%v", slots[0].Start, slots[0].End, from)
		}
		if slots[len(slots)-1].End.Before(until) {
			propFatalf(t, iter, "last slot ends %v before until=%v", slots[len(slots)-1].End, until)
		}
		for i, s := range slots {
			if !s.Start.Before(s.End) {
				propFatalf(t, iter, "degenerate slot [%v, %v)", s.Start, s.End)
			}
			if i > 0 && !s.Start.Equal(slots[i-1].End) {
				propFatalf(t, iter, "gap between slot %d and %d: %v != %v", i-1, i, slots[i-1].End, s.Start)
			}
			if got := g.SlotContaining(s.Start); !got.Start.Equal(s.Start) || !got.End.Equal(s.End) {
				propFatalf(t, iter, "SlotContaining disagrees at %v", s.Start)
			}
			if nb := g.NextBoundary(s.Start); !nb.Equal(s.End) {
				propFatalf(t, iter, "NextBoundary(%v) = %v, want %v", s.Start, nb, s.End)
			}
		}
	}
}

// Property 5: the codec round-trips any valid snapshot byte-stably.
func TestProp_CodecRoundTrip(t *testing.T) {
	r := newPropRand()
	for iter := 0; iter < 200; iter++ {
		snap := randActiveSnapshot(t, r, iter)
		b1, err := EncodeSnapshot(snap)
		if err != nil {
			propFatalf(t, iter, "encode: %v", err)
		}
		decoded, err := DecodeSnapshot(b1)
		if err != nil {
			propFatalf(t, iter, "decode: %v", err)
		}
		b2, err := EncodeSnapshot(decoded)
		if err != nil {
			propFatalf(t, iter, "re-encode: %v", err)
		}
		if !bytes.Equal(b1, b2) {
			propFatalf(t, iter, "codec not byte-stable:\n b1=%s\n b2=%s", b1, b2)
		}
	}
}

// Exhaustive check that generated weekly policies always match the model's
// weekday convention (0=Sunday) at the grid level.
func TestProp_WeeklyBoundaryWeekday(t *testing.T) {
	r := newPropRand()
	for iter := 0; iter < 200; iter++ {
		day := r.IntN(7)
		tz := randTimezone(r)
		policy := RotationPolicy{Cadence: model.RotationWeekly,
			HandoffTime: "11:00", HandoffDay: &day}
		g, err := NewGrid(tz, policy)
		if err != nil {
			propFatalf(t, iter, "NewGrid: %v", err)
		}
		loc, _ := time.LoadLocation(tz)
		s := g.SlotContaining(randInstant(r, 2024, 2027))
		if got := s.Start.In(loc).Weekday(); got != time.Weekday(day) {
			propFatalf(t, iter, "boundary weekday = %v, want %v (tz=%s)", got, time.Weekday(day), tz)
		}
	}
}
