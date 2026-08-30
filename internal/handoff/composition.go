package handoff

import (
	"sort"
	"time"

	"github.com/tokayops/tokayops/internal/schedulerender"
)

// Kinds of on-call notification. The same spelling is the classifier's answer,
// part of the dedup key and the metric label, so it exists exactly once.
const (
	// kindHandoff is a shift changing hands: a different group, or an override
	// starting or ending.
	kindHandoff = "handoff"

	// kindAddedToActiveShift is someone joining the group already on duty -
	// the result of an edit, not of the rotation.
	kindAddedToActiveShift = "added_to_active_shift"
)

// composition is who is on duty, in the form the detector compares.
//
// The boundaries of the assignment are deliberately NOT part of it. A reanchor
// after a policy edit moves the grid boundary without changing who is on duty,
// and a schedule with a single group crosses a boundary every shift with the
// same people on it - both would look like a handoff if a timestamp were in
// here. The moment of activation is still needed, but for the other question:
// see occurrenceOf.
//
// Source and GroupID are both present because the user set alone is ambiguous:
// the group [alice] and an override putting alice on duty name the same person
// for entirely different reasons, and moving between them is a real handoff.
type composition struct {
	// Source is "rotation", "override", or "" when nobody is on duty.
	Source string

	// GroupID is the rotation group for a rotation, the logical override ID
	// for an override.
	GroupID string

	// UserIDs is sorted, so that one composition has one spelling.
	UserIDs []string
}

// observation is one schedule as one tick saw it: the composition, the three
// instants the messages print, and where the composition came from.
type observation struct {
	Composition     composition
	GridSlotStart   time.Time
	AssignmentStart time.Time
	AssignmentEnd   time.Time

	// RevisionID is the version of the configuration that put this composition
	// on duty: the override revision when an override names the person, the
	// schedule revision otherwise.
	//
	// It is provenance, not identity, and it belongs only to the occurrence.
	// AssignmentStart alone cannot carry it: editing an override leaves
	// valid_from where it was, so swapping the stand-in out and back produces
	// the same composition at the same instant twice, and the second - real -
	// notification would be swallowed by the first job's dedup key.
	RevisionID string
}

// observe reads the L1 layer of a projection.
//
// L2 takes no part in detection: there is no DM for coming on call at L2 today
// and this does not introduce one. A schedule with nobody on L1 - deleted, layer
// switched off, no groups - yields the empty composition, which is a state the
// detector stores rather than a row it skips.
func observe(sc schedulerender.ScheduleOnCall) observation {
	l1 := sc.OnCall.L1
	if l1 == nil {
		return observation{}
	}
	users := append([]string(nil), l1.UserIDs...)
	sort.Strings(users)

	// An override says which of ITS versions is in force; a rotation says which
	// revision of the schedule it is running.
	revisionID := l1.ScheduleRevisionID
	if l1.Source == schedulerender.SourceOverride {
		revisionID = l1.OverrideRevisionID
	}

	return observation{
		Composition: composition{
			Source:  l1.Source,
			GroupID: l1.GroupID,
			UserIDs: users,
		},
		GridSlotStart:   l1.GridSlotStart,
		AssignmentStart: l1.AssignmentStart,
		AssignmentEnd:   l1.AssignmentEnd,
		RevisionID:      revisionID,
	}
}

func (c composition) empty() bool {
	return c.Source == "" && c.GroupID == "" && len(c.UserIDs) == 0
}

func (c composition) equal(other composition) bool {
	if c.Source != other.Source || c.GroupID != other.GroupID || len(c.UserIDs) != len(other.UserIDs) {
		return false
	}
	for i := range c.UserIDs {
		if c.UserIDs[i] != other.UserIDs[i] {
			return false
		}
	}
	return true
}

// clone detaches the user slice. A composition is kept in the cache across
// ticks, and the slice it came from belongs to the projection of one tick.
func (c composition) clone() composition {
	c.UserIDs = append([]string(nil), c.UserIDs...)
	return c
}

// classify decides what happened between two observations of one schedule and
// who has to hear about it.
//
// prev nil means the schedule has never been observed in this process. That is
// not a transition and produces nothing: the notifier announces changes, and a
// schedule it has never seen has not changed. Without that rule every restart
// would DM every on-call person, one fan-out per schedule - and a maintenance
// window in which schedules are recreated by hand would do it worst of all.
//
// A previously observed EMPTY composition is a different matter and does
// notify: after a stretch with nobody on duty, coming on call is real news.
//
// Only people who were not on duty a moment ago are told. Someone who was on
// call and still is gets nothing - on [A,B] -> [B,C] the message goes to C
// alone, where the old detector told B they had just come on call while they
// were already mid-shift.
func classify(prev *composition, next observation) (kind string, notify []string) {
	if next.Composition.empty() {
		// Nobody is on duty. There is no "you are off call" message today and
		// §17 does not ask for one; the caller still records the empty state,
		// because that is what makes the next arrival a transition.
		return "", nil
	}
	if prev == nil {
		return "", nil
	}
	if prev.equal(next.Composition) {
		return "", nil
	}

	added := addedUsers(prev.UserIDs, next.Composition.UserIDs)

	// A different group identity, or a move between rotation and override, is
	// the shift changing hands.
	if prev.Source != next.Composition.Source || prev.GroupID != next.Composition.GroupID {
		return kindHandoff, added
	}

	// Same source, same group, different members: only an edit can do that -
	// the rotation changes group identity when it turns. Whoever was added
	// joined a shift already in progress; whoever was removed is told nothing.
	if len(added) == 0 {
		return "", nil
	}
	return kindAddedToActiveShift, added
}

// addedUsers is next minus prev, in next's order.
func addedUsers(prev, next []string) []string {
	was := make(map[string]bool, len(prev))
	for _, id := range prev {
		was[id] = true
	}
	var out []string
	for _, id := range next {
		if !was[id] {
			out = append(out, id)
		}
	}
	return out
}
