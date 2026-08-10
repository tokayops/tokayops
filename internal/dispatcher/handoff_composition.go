package dispatcher

import (
	"sort"
	"strings"
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
// see occurrenceKey.
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

// observation is one schedule as one tick saw it: the composition plus the
// three instants the messages print.
type observation struct {
	Composition     composition
	GridSlotStart   time.Time
	AssignmentStart time.Time
	AssignmentEnd   time.Time
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
	return observation{
		Composition: composition{
			Source:  l1.Source,
			GroupID: l1.GroupID,
			UserIDs: users,
		},
		GridSlotStart:   l1.GridSlotStart,
		AssignmentStart: l1.AssignmentStart,
		AssignmentEnd:   l1.AssignmentEnd,
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

// key is the composition as one string, for the dedup key.
func (c composition) key() string {
	return c.Source + "|" + c.GroupID + "|" + strings.Join(c.UserIDs, ",")
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
// schedule it has never seen has not changed. Without that rule the cutover
// would DM every on-call person in the middle of the maintenance window, one
// fan-out per schedule the operator recreates.
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

// occurrenceKey identifies one notification, which is not the same question the
// composition answers.
//
// A composition recurs: a rotation A -> B -> C comes back to A, and an edit
// [B] -> [B,D] -> [B] -> [B,D] comes back to [B,D]. If the earlier job were
// still pending or running, the second - entirely legitimate - notification
// would be swallowed by the unique index on dedup_key. So the key carries the
// moment the assignment took effect, which differs between two arrivals of the
// same composition, and the kind, without which a lingering
// added_to_active_shift would suppress the handoff that follows it.
//
// The moment lives here and not in the composition on purpose: put it there and
// a schedule with one group would DM its members at every shift boundary.
func occurrenceKey(kind, scheduleID string, next observation) string {
	return strings.Join([]string{
		kind,
		scheduleID,
		next.Composition.key(),
		next.AssignmentStart.UTC().Format(time.RFC3339),
	}, ":")
}
