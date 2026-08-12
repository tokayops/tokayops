package schedulerender

import "time"

// Shift is a run of adjacent assignments presented as one natural shift.
//
// It is a distinct type from Assignment on purpose: a merged run spans several
// grid slots, so its grid boundaries would no longer describe a single handoff
// interval, and giving it the same shape as an atomic assignment would invite
// a caller to read them as one.
//
// So it does not carry them. It used to expose GridSlotStart/End, SlotCount
// and RevisionIDs - provenance that reached the calendar DTO and was read by
// nobody. Where grid boundaries do matter, they come from LayerOnCall, which
// describes ONE slot and is what the notifier and the editor read.
type Shift struct {
	Layer   string
	Source  string
	GroupID string
	UserIDs []string

	Start time.Time
	End   time.Time

	OverrideID         string
	OverrideRevisionID string
}

// MergeAdjacent joins contiguous assignments that describe the same duty.
//
// The revision is not part of that identity. A metadata-only save - one that
// changes a Slack usergroup or a reason and provably cannot move the rotation
// - creates a new revision in the middle of a shift, and treating it as a
// boundary would tear the rendered shift in two over a change the user is
// entitled to consider invisible. Which revisions contributed is kept in
// RevisionIDs, where it is provenance rather than identity.
//
// The input is not modified.
func MergeAdjacent(assignments []Assignment) []Shift {
	var builders []*shiftBuilder
	for _, a := range assignments {
		if n := len(builders); n > 0 && builders[n-1].canExtend(a) {
			builders[n-1].extend(a)
			continue
		}
		builders = append(builders, newShiftBuilder(a))
	}

	out := make([]Shift, 0, len(builders))
	for _, b := range builders {
		out = append(out, b.shift)
	}
	return out
}

// shiftBuilder carries the one thing the finished Shift does not need to
// expose: which grid slot the last assignment came from, so that repeated
// pieces of one slot do not inflate SlotCount.
type shiftBuilder struct {
	shift Shift
}

func newShiftBuilder(a Assignment) *shiftBuilder {
	return &shiftBuilder{
		shift: Shift{
			Layer:              a.Layer,
			Source:             a.Source,
			GroupID:            a.GroupID,
			UserIDs:            append([]string(nil), a.UserIDs...),
			Start:              a.AssignmentStart,
			End:                a.AssignmentEnd,
			OverrideID:         a.OverrideID,
			OverrideRevisionID: a.OverrideRevisionID,
		},
	}
}

// canExtend reports whether an assignment continues the shift: same layer,
// same source, same group and members, the same override version when the
// source is an override, and no time between them.
func (b *shiftBuilder) canExtend(a Assignment) bool {
	s := &b.shift
	return s.Layer == a.Layer &&
		s.Source == a.Source &&
		s.GroupID == a.GroupID &&
		s.OverrideID == a.OverrideID &&
		s.OverrideRevisionID == a.OverrideRevisionID &&
		equalIDs(s.UserIDs, a.UserIDs) &&
		s.End.Equal(a.AssignmentStart)
}

func (b *shiftBuilder) extend(a Assignment) {
	b.shift.End = a.AssignmentEnd
}

// sameShift compares two shifts by what they mean rather than by where they
// came from: layer, source, group, members, both pairs of boundaries and the
// slot count, but NOT the revisions that produced them.
//
// This is what a metadata-only save has to preserve. Comparing RevisionIDs as
// well would make the property untestable by construction, since a save
// creates a revision by definition; whether the right revisions are recorded
// is a separate assertion.
//
// Unexported deliberately: nothing outside this package needs it yet, and an
// exported comparator is a promise to keep. If the preview flow turns out to
// want "would this save change anything visible", exporting it then is a
// one-line change.
func sameShift(a, b Shift) bool {
	return a.Layer == b.Layer &&
		a.Source == b.Source &&
		a.GroupID == b.GroupID &&
		a.OverrideID == b.OverrideID &&
		a.OverrideRevisionID == b.OverrideRevisionID &&
		equalIDs(a.UserIDs, b.UserIDs) &&
		a.Start.Equal(b.Start) &&
		a.End.Equal(b.End)
}

// sameShifts is sameShift over two sequences.
func sameShifts(a, b []Shift) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameShift(a[i], b[i]) {
			return false
		}
	}
	return true
}
