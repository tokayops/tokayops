package dispatcher

import (
	"errors"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// The boundary the builder exists to hold: who can be reached, who cannot, and
// the difference between not knowing and knowing that nobody can.

// fakeAddressBook answers with what a test says is linked, or refuses to answer
// at all.
type fakeAddressBook struct {
	linked map[string][]*model.ExternalIdentity
	err    error
	calls  int
}

func (f *fakeAddressBook) GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string][]*model.ExternalIdentity, len(userIDs))
	for _, id := range userIDs {
		if linked, ok := f.linked[id]; ok {
			out[id] = linked
		}
	}
	return out, nil
}

func identity(provider, external string) *model.ExternalIdentity {
	return &model.ExternalIdentity{Provider: provider, ExternalID: external}
}

// builderOver is a builder over a fixed address book. Slack and telegram carry
// a direct message, email is registered here and does not, and anything else -
// hipchat, say - this build has never heard of.
func builderOver(linked map[string][]*model.ExternalIdentity) announcementBuilder {
	return announcementBuilder{
		identities: &fakeAddressBook{linked: linked},
		providers: staticCapabilities{
			"slack":    {"dm", "channel"},
			"telegram": {"dm", "channel"},
			"email":    {"channel"},
		},
	}
}

// TestAnnouncementSkipsAreReportedWithOneReasonEach.
//
// A person left out is a person who came on call and was not told, so the
// reason is what an operator has to act on. Somebody can answer to more than
// one at once, and the answer has to be the same every time - two instances
// walking the same links in different orders must not produce two series for
// one skip.
//
// The four are separate because they send whoever reads them to four different
// places: a channel that does not do direct messages is a setting, a channel
// this build has never heard of was removed from under a link, an empty address
// is a link half made, and no links at all is a person who made none.
func TestAnnouncementSkipsAreReportedWithOneReasonEach(t *testing.T) {
	cases := []struct {
		name   string
		linked []*model.ExternalIdentity
		want   skipReason
	}{
		{
			name: "nothing linked at all",
			want: skipNoIdentity,
		},
		{
			name:   "a link that was started and not finished",
			linked: []*model.ExternalIdentity{identity("slack", "")},
			want:   skipIdentityIncomplete,
		},
		{
			name:   "a channel that was taken away from under the link",
			linked: []*model.ExternalIdentity{identity("hipchat", "H-BOB")},
			want:   skipUnknownProvider,
		},
		{
			name:   "a channel that is here and writes to nobody in private",
			linked: []*model.ExternalIdentity{identity("email", "b@example.test")},
			want:   skipNoDMCapability,
		},
		{
			// Three at once. The channel that could be configured to carry a
			// direct message is the answer, because it is the one somebody can
			// change without touching the person or their links.
			name: "a setting, a removal and half a link",
			linked: []*model.ExternalIdentity{
				identity("hipchat", "H-BOB"),
				identity("slack", ""),
				identity("email", "b@example.test"),
			},
			want: skipNoDMCapability,
		},
		{
			// The removed channel outranks the half-made link for the same
			// reason: nobody finishing that link would help, because what it
			// points at is gone.
			name: "a removal and half a link",
			linked: []*model.ExternalIdentity{
				identity("slack", ""),
				identity("hipchat", "H-BOB"),
			},
			want: skipUnknownProvider,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := builderOver(map[string][]*model.ExternalIdentity{"bob": tc.linked})
			batch, left, err := b.build(rotationDuty("sched-1", "g-b", "bob"), kindHandoff,
				[]string{"bob"}, observe(rotationDuty("sched-1", "g-b", "bob")), "Backend")
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if promised := len(batch.Admission.Commitments); promised != 0 {
				t.Fatalf("promised %d messages to somebody nothing can reach", promised)
			}
			if len(left) != 1 {
				t.Fatalf("reported %d people left out, want 1: %+v", len(left), left)
			}
			if left[0].UserID != "bob" || left[0].Reason != tc.want {
				t.Errorf("left out %+v, want bob for %q", left[0], tc.want)
			}
		})
	}
}

// TestAnAnnouncementNobodyCanReceiveIsStillAnAnnouncement.
//
// Nobody reachable is an ANSWER about the occurrence, and it is offered as one:
// a claim with no commitments under it. Staying quiet instead would leave the
// occurrence unclaimed, and an instance with a different view of who is linked
// could then promise something under it minutes later.
func TestAnAnnouncementNobodyCanReceiveIsStillAnAnnouncement(t *testing.T) {
	b := builderOver(nil)
	batch, left, err := b.build(rotationDuty("sched-1", "g-b", "bob", "carol"), kindHandoff,
		[]string{"bob", "carol"}, observe(rotationDuty("sched-1", "g-b", "bob", "carol")), "Backend")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if batch.Admission.BatchKey == "" {
		t.Fatal("no claim was offered over the occurrence")
	}
	if promised := len(batch.Admission.Commitments); promised != 0 {
		t.Fatalf("promised %d messages with nobody reachable", promised)
	}
	if len(left) != 2 {
		t.Fatalf("reported %d people left out, want both: %+v", len(left), left)
	}
}

// TestAFailedIdentityReadIsNotAnEmptyAnswer.
//
// The two look identical from one line down: a lookup that failed and a lookup
// that found nobody both leave an empty set. Their consequences do not. The
// second is a durable statement that there was nobody to tell, and the claim
// backing it is held forever - so a read failure passed off as one would spend
// the occurrence on a database hiccup, and the real announcement could never be
// admitted afterwards.
func TestAFailedIdentityReadIsNotAnEmptyAnswer(t *testing.T) {
	b := announcementBuilder{
		identities: &fakeAddressBook{err: errors.New("connection reset")},
		providers:  staticDmProviders("slack"),
	}

	batch, left, err := b.build(rotationDuty("sched-1", "g-b", "bob"), kindHandoff,
		[]string{"bob"}, observe(rotationDuty("sched-1", "g-b", "bob")), "Backend")
	if err == nil {
		t.Fatal("a failed read produced an announcement")
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("the failure does not say what went wrong: %v", err)
	}
	if batch.Admission.BatchKey != "" || left != nil {
		t.Errorf("a failed read produced %+v and %+v", batch, left)
	}
}

// TestAnnouncementFansOutPerProviderInOneOrder. One person on two channels is
// two promises, and the order they are built in is fixed: the commitment keys
// are what two instances racing on one occurrence compare, and a set that
// depended on map order would make the same work look like different work.
func TestAnnouncementFansOutPerProviderInOneOrder(t *testing.T) {
	b := builderOver(map[string][]*model.ExternalIdentity{
		"bob": {identity("telegram", "T-BOB"), identity("slack", "S-BOB")},
	})

	batch, left, err := b.build(rotationDuty("sched-1", "g-b", "bob"), kindHandoff,
		[]string{"bob"}, observe(rotationDuty("sched-1", "g-b", "bob")), "Backend")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("left somebody out who is linked twice: %+v", left)
	}

	var providers []string
	for _, c := range batch.Admission.Commitments {
		providers = append(providers, c.Provider)
		if c.Target.Ref != "bob" {
			t.Errorf("a commitment names %q, want the person and not an account", c.Target.Ref)
		}
	}
	if strings.Join(providers, ",") != "slack,telegram" {
		t.Errorf("fan-out order = %v, want it fixed", providers)
	}
}

// TestAKindTheGrammarDoesNotNameIsNotAnnounced. The detector's vocabulary and
// the grammar's are two lists that agree today. Mapping an unknown kind onto
// the ordinary handover would announce the wrong event, under a key that
// collides with the right one.
func TestAKindTheGrammarDoesNotNameIsNotAnnounced(t *testing.T) {
	b := builderOver(map[string][]*model.ExternalIdentity{
		"bob": {identity("slack", "S-BOB")},
	})

	_, _, err := b.build(rotationDuty("sched-1", "g-b", "bob"), "shift_shortened",
		[]string{"bob"}, observe(rotationDuty("sched-1", "g-b", "bob")), "Backend")
	if err == nil {
		t.Fatal("a kind the grammar does not name was announced")
	}
	if !strings.Contains(err.Error(), "shift_shortened") {
		t.Errorf("the refusal does not name the kind: %v", err)
	}
}

// TestTheAnnouncementIsAboutTheScheduleItWasBuiltFrom is a guard on the one
// field nothing else here would catch: every other part of the occurrence comes
// from the observation, and the schedule id comes from the projection beside
// it. Crossed, two schedules changing shift at the same instant would share a
// claim and one of them would go unannounced.
func TestTheAnnouncementIsAboutTheScheduleItWasBuiltFrom(t *testing.T) {
	b := builderOver(map[string][]*model.ExternalIdentity{
		"bob": {identity("slack", "S-BOB")},
	})
	sc := rotationDuty("sched-1", "g-b", "bob")

	batch, _, err := b.build(sc, kindHandoff, []string{"bob"}, observe(sc), "Backend")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	other := rotationDuty("sched-2", "g-b", "bob")
	elsewhere, _, err := b.build(other, kindHandoff, []string{"bob"}, observe(other), "Backend")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if batch.Admission.BatchKey == elsewhere.Admission.BatchKey {
		t.Fatalf("two schedules share the claim %q", batch.Admission.BatchKey)
	}
}
