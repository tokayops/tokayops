package keys

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// What version 2 added: the history and the buttons in the snapshot, one
// digest per form, and every field from outside cut before the digest.

func digests(t *testing.T, s RenderSnapshot) (snapshot, card, thread string) {
	t.Helper()
	return mustDigest(t, s), hex.EncodeToString(s.CardDigest()), hex.EncodeToString(s.ThreadDigest())
}

// lineOf finds a line of history by id: the fixture is deliberately out of
// order, so a canonical snapshot cannot be indexed the way its input was.
func lineOf(t *testing.T, s SnapshotInput, id string) TimelineEventSnapshot {
	t.Helper()
	for _, line := range s.Timeline {
		if line.ID == id {
			return line
		}
	}
	t.Fatalf("no line %s in the snapshot", id)
	return TimelineEventSnapshot{}
}

// TestTheThreeDigestsSeeDifferentFields is the rule a revision is raised by:
// a field one form does not render does not live in that form's digest. A
// note changes the thread and not the card; the button switch changes the
// card and not the thread; a new alert and the zone change both; and the
// order a producer listed things in changes nothing at all.
func TestTheThreeDigestsSeeDifferentFields(t *testing.T) {
	base := mustSnapshot(t, fixtureSnapshot())
	snapshot0, card0, thread0 := digests(t, base)

	t.Run("a note reaches the thread only", func(t *testing.T) {
		in := fixtureSnapshot()
		in.Timeline = append(in.Timeline, TimelineEventSnapshot{
			ID: "e-4", Type: EventNote, Message: "escalating", Actor: str("nina"),
			CreatedAt: fixtureStart.Add(5 * time.Minute),
		})
		snapshot, card, thread := digests(t, mustSnapshot(t, in))
		if snapshot == snapshot0 || thread == thread0 {
			t.Fatal("a new line of history did not change the snapshot and the thread")
		}
		if card != card0 {
			t.Fatal("a new line of history changed the card")
		}
	})

	t.Run("the button switch reaches the card only", func(t *testing.T) {
		in := fixtureSnapshot()
		in.InteractiveProviders = []string{InteractiveTelegram}
		snapshot, card, thread := digests(t, mustSnapshot(t, in))
		if snapshot == snapshot0 || card == card0 {
			t.Fatal("switching buttons off did not change the snapshot and the card")
		}
		if thread != thread0 {
			t.Fatal("switching buttons off changed the thread")
		}
	})

	for name, change := range map[string]func(*SnapshotInput){
		"a new alert": func(s *SnapshotInput) {
			s.Alerts = append(s.Alerts, AlertSnapshot{
				Fingerprint: "fp-3", Status: AlertFiring, StartsAt: fixtureStart.Add(2 * time.Minute),
				AlertName: "DiskGone", Severity: "critical",
			})
		},
		"the zone":     func(s *SnapshotInput) { s.DisplayTimezone = "Europe/Berlin" },
		"the revision": func(s *SnapshotInput) { s.Revision = 1 },
	} {
		t.Run(name+" reaches every digest", func(t *testing.T) {
			in := fixtureSnapshot()
			change(&in)
			snapshot, card, thread := digests(t, mustSnapshot(t, in))
			if snapshot == snapshot0 || card == card0 || thread == thread0 {
				t.Fatalf("%s left a digest unchanged", name)
			}
		})
	}

	t.Run("the input order is nobody's business", func(t *testing.T) {
		in := fixtureSnapshot()
		in.Alerts = []AlertSnapshot{in.Alerts[1], in.Alerts[0]}
		in.Timeline = []TimelineEventSnapshot{in.Timeline[2], in.Timeline[0], in.Timeline[1]}
		in.InteractiveProviders = []string{InteractiveSlack, InteractiveTelegram}
		shuffled := mustSnapshot(t, in)
		snapshot, card, thread := digests(t, shuffled)
		if snapshot != snapshot0 || card != card0 || thread != thread0 {
			t.Fatal("reordering the input changed a digest")
		}
		want, _ := json.Marshal(base)
		got, _ := json.Marshal(shuffled)
		if string(got) != string(want) {
			t.Fatalf("reordering the input changed the stored form:\n%s\n%s", got, want)
		}
	})
}

// TestTheHistoryKeepsTheLastTwenty. The snapshot holds what the thread shows
// and nothing more; what it does not hold is counted, and the count is what
// the thread says at the top. Cutting again changes nothing, or a stored
// snapshot would not survive its round trip through storage.
func TestTheHistoryKeepsTheLastTwenty(t *testing.T) {
	in := fixtureSnapshot()
	in.Timeline = nil
	for i := 0; i < TimelineLength+5; i++ {
		in.Timeline = append(in.Timeline, TimelineEventSnapshot{
			ID: "e-" + strings.Repeat("x", i+1), Type: EventNote, Message: "n",
			CreatedAt: fixtureStart.Add(time.Duration(TimelineLength+5-i) * time.Minute),
		})
	}
	in.TimelineOmitted = 3

	kept := mustSnapshot(t, in).Content()
	if len(kept.Timeline) != TimelineLength {
		t.Fatalf("%d lines were kept", len(kept.Timeline))
	}
	if kept.TimelineOmitted != 8 {
		t.Fatalf("%d lines were counted as omitted, want 3 handed over plus 5 cut", kept.TimelineOmitted)
	}
	// The most recent ones, oldest first: the five earliest went.
	if first := kept.Timeline[0].CreatedAt; !first.Equal(fixtureStart.Add(6 * time.Minute)) {
		t.Fatalf("the oldest kept line is at %v", first)
	}
	for i := 1; i < len(kept.Timeline); i++ {
		if !kept.Timeline[i-1].CreatedAt.Before(kept.Timeline[i].CreatedAt) {
			t.Fatal("the kept lines are not oldest first")
		}
	}

	again := mustSnapshot(t, kept).Content()
	if again.TimelineOmitted != 8 || len(again.Timeline) != TimelineLength {
		t.Fatal("cutting an already-cut history changed it")
	}

	exact := fixtureSnapshot()
	exact.Timeline = in.Timeline[:TimelineLength]
	exact.TimelineOmitted = 1
	if got := mustSnapshot(t, exact).Content().TimelineOmitted; got != 1 {
		t.Fatalf("a history that fits had its count changed to %d", got)
	}
}

// TestTheOrderOfHistoryIsTotal: by time, then by id, so two lines written in
// the same instant have one order everywhere.
func TestTheOrderOfHistoryIsTotal(t *testing.T) {
	in := fixtureSnapshot()
	in.Timeline = []TimelineEventSnapshot{
		{ID: "e-b", Type: EventNote, Message: "b", CreatedAt: fixtureStart},
		{ID: "e-a", Type: EventNote, Message: "a", CreatedAt: fixtureStart},
	}
	lines := mustSnapshot(t, in).Content().Timeline
	if lines[0].ID != "e-a" || lines[1].ID != "e-b" {
		t.Fatalf("two lines of one instant came out as %s, %s", lines[0].ID, lines[1].ID)
	}
}

// TestNobodyIsNoActor. A line written by the system and a line with no actor
// are both lines nobody wrote, and the thread prints neither name. They are
// the same snapshot.
func TestNobodyIsNoActor(t *testing.T) {
	// The fixture's first input line is e-3.
	with := func(actor *string) RenderSnapshot {
		in := fixtureSnapshot()
		in.Timeline[0].Actor = actor
		return mustSnapshot(t, in)
	}
	system, empty, none := with(str(SystemActor)), with(str("")), with(nil)
	if mustDigest(t, system) != mustDigest(t, none) || mustDigest(t, empty) != mustDigest(t, none) {
		t.Fatal("nobody was somebody")
	}
	if lineOf(t, system.Content(), "e-3").Actor != nil {
		t.Fatal("the system was stored as an actor")
	}
	if got := lineOf(t, with(str("nina")).Content(), "e-3").Actor; got == nil || *got != "nina" {
		t.Fatal("a person was lost")
	}
}

// TestEveryOutsideFieldIsCutBeforeTheDigest extends the rule the description
// had to every field that arrives from outside. Two values that share their
// first N runes render byte for byte the same, so they have to be one
// snapshot; and cutting what was already cut changes nothing.
func TestEveryOutsideFieldIsCutBeforeTheDigest(t *testing.T) {
	cases := []struct {
		name  string
		limit int
		set   func(*SnapshotInput, string)
		get   func(SnapshotInput) string
	}{
		{"title", TitleLimit,
			func(s *SnapshotInput, v string) { s.Title = v },
			func(s SnapshotInput) string { return s.Title }},
		{"alert name", AlertNameLimit,
			func(s *SnapshotInput, v string) { s.Alerts[0].AlertName = v },
			func(s SnapshotInput) string { return s.Alerts[0].AlertName }},
		{"description", AlertDescriptionLimit,
			func(s *SnapshotInput, v string) { s.Alerts[0].Description = &v },
			func(s SnapshotInput) string { return *s.Alerts[0].Description }},
		{"history message", TimelineMessageLimit,
			func(s *SnapshotInput, v string) { s.Timeline[0].Message = v },
			func(s SnapshotInput) string { return lineOf(t, s, "e-3").Message }},
		{"history actor", ActorLimit,
			func(s *SnapshotInput, v string) { s.Timeline[2].Actor = &v },
			func(s SnapshotInput) string { return *lineOf(t, s, "e-2").Actor }},
		{"acknowledged by", ActorLimit,
			func(s *SnapshotInput, v string) { s.AcknowledgedBy = &v },
			func(s SnapshotInput) string { return *s.AcknowledgedBy }},
		{"resolved by", ActorLimit,
			func(s *SnapshotInput, v string) { s.ResolvedBy = &v },
			func(s SnapshotInput) string { return *s.ResolvedBy }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := strings.Repeat("\u00e9", tc.limit) // runes, not bytes
			with := func(tail string) RenderSnapshot {
				in := fixtureSnapshot()
				tc.set(&in, head+tail)
				return mustSnapshot(t, in)
			}
			first, second := with("...and then some"), with("...and then something else")
			if mustDigest(t, first) != mustDigest(t, second) {
				t.Fatalf("two %ss that render the same produced different digests", tc.name)
			}
			stored := tc.get(first.Content())
			if stored != head+AlertDescriptionEllipsis {
				t.Fatalf("the stored %s is %q", tc.name, stored)
			}

			again := fixtureSnapshot()
			tc.set(&again, stored)
			if mustDigest(t, mustSnapshot(t, again)) != mustDigest(t, first) {
				t.Fatalf("cutting an already-cut %s changed the snapshot", tc.name)
			}

			fits := fixtureSnapshot()
			tc.set(&fits, head)
			if got := tc.get(mustSnapshot(t, fits).Content()); got != head {
				t.Fatalf("a %s that fits came back as %q", tc.name, got)
			}
		})
	}
}

// TestButtonProvidersAreOneSortedList. The list is an identity, so the order a
// producer named the providers in - or naming one twice - is not part of it.
func TestButtonProvidersAreOneSortedList(t *testing.T) {
	in := fixtureSnapshot()
	in.InteractiveProviders = []string{InteractiveTelegram, InteractiveSlack, InteractiveSlack}
	got := mustSnapshot(t, in).Content().InteractiveProviders
	if len(got) != 2 || got[0] != InteractiveSlack || got[1] != InteractiveTelegram {
		t.Fatalf("the providers came back as %v", got)
	}

	in.InteractiveProviders = nil
	if got := mustSnapshot(t, in).Content().InteractiveProviders; got == nil || len(got) != 0 {
		t.Fatalf("no providers came back as %#v", got)
	}
}
