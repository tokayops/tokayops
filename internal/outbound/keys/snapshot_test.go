package keys

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func str(v string) *string { return &v }

var (
	fixtureStart = time.Unix(1700000000, 0).UTC()
	fixtureEvent = time.Unix(1700000060, 0).UTC()
)

func fixtureSnapshot() SnapshotInput {
	return SnapshotInput{
		AlertGroupID:    fixtureGroup,
		Revision:        0,
		Status:          GroupProcessing,
		Title:           "disk filling up",
		Severity:        "critical",
		TeamLabel:       str("platform"),
		TeamOnboarded:   true,
		GroupURL:        str("https://tokay.example/#/ops/alert-groups/ag-0001"),
		ExternalURL:     str("https://alerts.example/graph"),
		DisplayTimezone: "UTC",
		Alerts: []AlertSnapshot{
			{
				Fingerprint: "fp-1", Status: AlertFiring, StartsAt: fixtureStart,
				AlertName: "DiskWillFill", Severity: "critical",
				SlackUser: str("U0001"), Description: str("12% left"),
			},
			{
				Fingerprint: "fp-2", Status: AlertResolved,
				StartsAt: fixtureStart.Add(time.Minute), AlertName: "DiskSlow",
				Severity: "warning",
			},
		},
		TeamSetupURL: str("https://tokay.example/#/cfg/teams"),
		// Out of order on purpose, with the two spellings of nobody: the
		// canonical form is what the tests below pin.
		Timeline: []TimelineEventSnapshot{
			{ID: "e-3", Type: EventNote, Message: "looking into it", Actor: str(""),
				CreatedAt: fixtureStart.Add(3 * time.Minute)},
			{ID: "e-1", Type: EventCreated, Message: "Alert group created", Actor: str(SystemActor),
				CreatedAt: fixtureStart},
			{ID: "e-2", Type: EventAcknowledged, Message: "Acknowledged by nina", Actor: str("nina"),
				CreatedAt: fixtureStart.Add(2 * time.Minute)},
		},
		TimelineOmitted:      2,
		InteractiveProviders: []string{InteractiveTelegram, InteractiveSlack},
	}
}

// fixtureV1JSON is the fixture as version 1 stored it: the same fields minus
// the three that arrived with version 2.
func fixtureV1JSON(t *testing.T) []byte {
	t.Helper()
	in := fixtureSnapshot()
	raw, err := json.Marshal(snapshotInputV1{
		AlertGroupID: in.AlertGroupID, Revision: in.Revision, Status: in.Status,
		Title: in.Title, Severity: in.Severity, TeamLabel: in.TeamLabel,
		TeamOnboarded: in.TeamOnboarded, GroupURL: in.GroupURL, ExternalURL: in.ExternalURL,
		DisplayTimezone: in.DisplayTimezone, AcknowledgedBy: in.AcknowledgedBy,
		ResolvedBy: in.ResolvedBy, Alerts: in.Alerts, TeamSetupURL: in.TeamSetupURL,
	})
	if err != nil {
		t.Fatalf("store the version 1 fixture: %v", err)
	}
	return raw
}

func mustSnapshot(t *testing.T, in SnapshotInput) RenderSnapshot {
	t.Helper()
	out, err := NewRenderSnapshot(in)
	if err != nil {
		t.Fatalf("build the snapshot: %v", err)
	}
	return out
}

func mustDigest(t *testing.T, s RenderSnapshot) string {
	t.Helper()
	return hex.EncodeToString(s.Digest())
}

// TestRenderSnapshotDigestIsGolden pins the content identity of a revision,
// and the two per-form digests beside it.
//
// The snapshot digest travels in the key of every commitment the revision
// produced, so changing it changes what those commitments are about - and a
// producer repeating an admission would be told its own proposal conflicts.
//
// The vectors were computed from the written protocol by hand, not read off
// this implementation. They moved twice: on 2026-08-25, when tag 14 (the
// alert's history) was retired from version 1, and on 2026-09-05, when
// version 2 brought the history back as tags 16-17, added the buttons as tag
// 18 and gave each form a digest. Those are the only reasons a digest is
// allowed to move: one that changes for any other reason is a bug, not a
// refactor. Version 1's own bytes are pinned below, through its reader.
func TestRenderSnapshotDigestIsGolden(t *testing.T) {
	snapshot := mustSnapshot(t, fixtureSnapshot())
	for name, tc := range map[string]struct{ got, want string }{
		"snapshot": {mustDigest(t, snapshot),
			"cddd4e2fa6a3a2e9199858b920a659be9fd5c2626d17d3bec1b0406c9ba77b70"},
		"card": {hex.EncodeToString(snapshot.CardDigest()),
			"77545ee60fce971297513602a2ad68d8025ad4a512e873f03ba2b313b0d3deef"},
		"thread": {hex.EncodeToString(snapshot.ThreadDigest()),
			"4175dd676450514f57e77471be9a02bfdb9b5d1480739224ec52f72a2b0eda00"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s digest\n got: %s\nwant: %s", name, tc.got, tc.want)
		}
	}
	if snapshot.SchemaVersion() != RenderSnapshotSchemaV2 {
		t.Fatalf("a snapshot built here is version %d", snapshot.SchemaVersion())
	}
}

// TestAVersionOneSnapshotStillDigestsToItsOldBytes is the other half of the
// golden vector: the digest a version 1 row was keyed against is what its
// reader computes, byte for byte, or every admission frozen before the upgrade
// would stop matching its own claim.
//
// The reader is for reading. It canonicalises by version 1's rules - a long
// alert name is stored whole, because that is what version 1 hashed - and what
// it returns has no per-form digests and cannot be written back.
func TestAVersionOneSnapshotStillDigestsToItsOldBytes(t *testing.T) {
	older, err := DecodeRenderSnapshotV1(fixtureV1JSON(t))
	if err != nil {
		t.Fatalf("read the version 1 row: %v", err)
	}
	if got, want := mustDigest(t, older),
		"823f2da6ea49a29a2da9175e0e0c2dd5552d30943104690bb3bed5d704f4d08e"; got != want {
		t.Fatalf("version 1 digest\n got: %s\nwant: %s", got, want)
	}
	if older.SchemaVersion() != RenderSnapshotSchemaV1 {
		t.Fatalf("the row came back as version %d", older.SchemaVersion())
	}
	if older.CardDigest() != nil || older.ThreadDigest() != nil {
		t.Fatal("a version 1 snapshot has per-form digests")
	}
	if content := older.Content(); len(content.Timeline) != 0 || len(content.InteractiveProviders) != 0 {
		t.Fatal("a version 1 snapshot came back with a history or buttons")
	}
	if _, err := json.Marshal(older); err == nil {
		t.Fatal("a version 1 snapshot was written back")
	}

	// Version 1 stored names whole. The reader must not cut them: the digest
	// column beside the row was taken over the whole name.
	in := fixtureSnapshot()
	in.Alerts[0].AlertName = strings.Repeat("n", AlertNameLimit+50)
	rawLong, err := json.Marshal(snapshotInputV1{
		AlertGroupID: in.AlertGroupID, Status: in.Status, Title: in.Title,
		Severity: in.Severity, DisplayTimezone: in.DisplayTimezone, Alerts: in.Alerts,
	})
	if err != nil {
		t.Fatalf("store the long name: %v", err)
	}
	long, err := DecodeRenderSnapshotV1(rawLong)
	if err != nil {
		t.Fatalf("read the long name: %v", err)
	}
	if got := long.Content().Alerts[0].AlertName; len([]rune(got)) != AlertNameLimit+50 {
		t.Fatalf("the version 1 reader cut a name to %d runes", len([]rune(got)))
	}

	// And a row carrying what version 1 never stored is not a version 1 row.
	for name, extra := range map[string]string{
		"a history":        `"timeline":[]`,
		"an omitted count": `"timeline_omitted":0`,
		"buttons":          `"interactive_providers":[]`,
	} {
		raw := fixtureV1JSON(t)
		raw = append(raw[:len(raw)-1], []byte(","+extra+"}")...)
		if _, err := DecodeRenderSnapshotV1(raw); err == nil {
			t.Errorf("a version 1 reader accepted a row with %s", name)
		}
	}

	// Admission is made from the current shape only: what a version 1 row
	// says was admitted once is not something to admit again.
	batch := EscalationBatch{
		Kind: KindEscalation, GrammarVersion: GrammarV1,
		FingerprintVersion: CurrentBatchFingerprintVersion(), Snapshot: older,
	}
	if _, err := batch.Admit(); err == nil {
		t.Fatal("a batch was admitted from a version 1 snapshot")
	}
}

// TestRenderSnapshotCanonicalisesOnce is the rule that keeps a digest honest:
// the stored order, the hashed order and the rendered order are one order,
// settled when the snapshot is built. Sorting only before hashing would let two
// snapshots share a digest and still produce different messages.
func TestRenderSnapshotCanonicalisesOnce(t *testing.T) {
	forward := mustSnapshot(t, fixtureSnapshot())

	extra := AlertSnapshot{
		Fingerprint: "fp-0", Status: AlertFiring,
		StartsAt: fixtureStart.Add(-time.Minute), AlertName: "DiskWarm",
		Severity: "warning",
	}

	shuffled := fixtureSnapshot()
	shuffled.Alerts = []AlertSnapshot{shuffled.Alerts[1], extra, shuffled.Alerts[0]}
	backward := mustSnapshot(t, shuffled)

	forwardWithExtra := fixtureSnapshot()
	forwardWithExtra.Alerts = append([]AlertSnapshot{extra}, forwardWithExtra.Alerts...)
	expected := mustSnapshot(t, forwardWithExtra)

	if mustDigest(t, backward) != mustDigest(t, expected) {
		t.Fatal("the same content in a different input order produced a different digest")
	}
	if first := backward.Content().Alerts[0].Fingerprint; first != "fp-0" {
		t.Fatalf("alerts were not canonicalised: first is %s", first)
	}

	// And the stored order is what a renderer will read back.
	if mustDigest(t, forward) == mustDigest(t, backward) {
		t.Fatal("adding an alert did not change the digest")
	}
}

// TestALongDescriptionIsCutBeforeTheDigest is the rule that keeps the digest
// and the message agreeing about one field.
//
// Two alerts whose descriptions share their first 120 runes and differ after
// them render byte for byte the same. If the cut happened at render time, they
// would still hold different digests - and the difference nobody can see would
// raise a revision and send a real edit. That is exactly why the history left
// this protocol, and a field cut in the wrong place brings it back.
func TestALongDescriptionIsCutBeforeTheDigest(t *testing.T) {
	head := strings.Repeat("x", AlertDescriptionLimit)

	with := func(tail string) RenderSnapshot {
		in := fixtureSnapshot()
		full := head + tail
		in.Alerts[0].Description = &full
		return mustSnapshot(t, in)
	}

	first := with("...and then some")
	second := with("...and then something else entirely")

	if mustDigest(t, first) != mustDigest(t, second) {
		t.Fatal("two descriptions that render the same produced different digests")
	}

	stored := *first.Content().Alerts[0].Description
	if stored != head+AlertDescriptionEllipsis {
		t.Fatalf("the stored description is %q", stored)
	}
	if *second.Content().Alerts[0].Description != stored {
		t.Fatal("the two snapshots stored different descriptions")
	}

	// Canonicalising what was already canonicalised has to change nothing, or
	// a snapshot would not survive the round trip through storage it makes
	// before every attempt.
	again := fixtureSnapshot()
	again.Alerts[0].Description = &stored
	if mustDigest(t, mustSnapshot(t, again)) != mustDigest(t, first) {
		t.Fatal("cutting an already-cut description changed the snapshot")
	}

	// And a description that fits is left exactly as it came.
	short := "12% left"
	fits := fixtureSnapshot()
	fits.Alerts[0].Description = &short
	if got := *mustSnapshot(t, fits).Content().Alerts[0].Description; got != short {
		t.Fatalf("a description that fits came back as %q", got)
	}
}

// TestRenderSnapshotIgnoresTheProcessZone proves the digest describes instants
// rather than how a particular instance would print them. Two instances in
// different time zones have to agree about what was accepted; how it is
// displayed is carried separately, by DisplayTimezone.
func TestRenderSnapshotIgnoresTheProcessZone(t *testing.T) {
	somewhere := time.FixedZone("UTC+7", 7*60*60)

	shifted := fixtureSnapshot()
	for i := range shifted.Alerts {
		shifted.Alerts[i].StartsAt = shifted.Alerts[i].StartsAt.In(somewhere)
	}

	if mustDigest(t, mustSnapshot(t, shifted)) != mustDigest(t, mustSnapshot(t, fixtureSnapshot())) {
		t.Fatal("the same instants in another zone produced a different digest")
	}
}

// TestRenderSnapshotSurvivesStorage checks the one round trip that happens for
// real: the snapshot is stored as JSON and read back before every attempt. If
// that trip changed the digest, the commitment would stop matching its own key.
func TestRenderSnapshotSurvivesStorage(t *testing.T) {
	stored := mustSnapshot(t, fixtureSnapshot())

	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatalf("store the snapshot: %v", err)
	}
	var read RenderSnapshot
	if err := json.Unmarshal(encoded, &read); err != nil {
		t.Fatalf("read the snapshot back: %v", err)
	}

	if mustDigest(t, read) != mustDigest(t, stored) {
		t.Fatal("a stored snapshot came back with a different digest")
	}

	// A row that no longer canonicalises has to fail here rather than be
	// rendered into a message whose content its key does not describe.
	var corrupted RenderSnapshot
	if err := json.Unmarshal([]byte(`{"alert_group_id":"ag-1","status":"firing",`+
		`"display_timezone":"UTC","alerts":[]}`), &corrupted); err == nil {
		t.Fatal("a stored snapshot with a status from nowhere was read back happily")
	}

	// The stored shape has one spelling: a snapshot with no history and no
	// buttons still writes the three fields, so a version 2 row is never
	// mistaken for a version 1 row by its bytes alone.
	bare := fixtureSnapshot()
	bare.Timeline, bare.TimelineOmitted, bare.InteractiveProviders = nil, 0, nil
	written, err := json.Marshal(mustSnapshot(t, bare))
	if err != nil {
		t.Fatalf("store the bare snapshot: %v", err)
	}
	for _, field := range []string{`"timeline":[]`, `"timeline_omitted":0`, `"interactive_providers":[]`} {
		if !strings.Contains(string(written), field) {
			t.Errorf("a stored snapshot lacks %s:\n%s", field, written)
		}
	}
}

// TestRenderSnapshotCannotBeReachedAround checks the other half of the same
// guarantee: what a renderer is handed is a copy, so nothing it does can move
// the value the digest was taken over.
func TestRenderSnapshotCannotBeReachedAround(t *testing.T) {
	snapshot := mustSnapshot(t, fixtureSnapshot())
	before := mustDigest(t, snapshot)

	content := snapshot.Content()
	content.Alerts[0].AlertName = "something else"
	content.Alerts = nil

	if mustDigest(t, snapshot) != before {
		t.Fatal("editing a rendered copy changed the snapshot it came from")
	}
}

// TestRenderSnapshotDoesNotShareItsOptionals is the aliasing case, and it is
// the quiet one: copying the slices is not enough, because every optional value
// inside them is a pointer. A caller that edits one afterwards would change what
// the next attempt renders while the digest went on describing what was
// accepted.
func TestRenderSnapshotDoesNotShareItsOptionals(t *testing.T) {
	label := "platform"
	description := "12% left"

	in := fixtureSnapshot()
	in.TeamLabel = &label
	in.Alerts[0].Description = &description

	snapshot := mustSnapshot(t, in)
	before := mustDigest(t, snapshot)

	// The caller edits what it still holds.
	label = "billing"
	description = "3% left"

	if got := *snapshot.Content().TeamLabel; got != "platform" {
		t.Fatalf("the snapshot followed an edit of its input: team label is %q", got)
	}
	if got := *snapshot.Content().Alerts[0].Description; got != "12% left" {
		t.Fatalf("the snapshot followed an edit of its input: description is %q", got)
	}
	if mustDigest(t, snapshot) != before {
		t.Fatal("the digest moved")
	}

	// And what a renderer is handed cannot be used to reach back either.
	content := snapshot.Content()
	*content.TeamLabel = "somewhere else"
	*content.Alerts[0].Description = "nothing left"

	if got := *snapshot.Content().TeamLabel; got != "platform" {
		t.Fatalf("a rendered copy reached back into the snapshot: team label is %q", got)
	}
	if got := *snapshot.Content().Alerts[0].Description; got != "12% left" {
		t.Fatalf("a rendered copy reached back into the snapshot: description is %q", got)
	}
	if mustDigest(t, snapshot) != before {
		t.Fatal("the digest moved")
	}
}

// TestRenderSnapshotRefusesAFutureSchema covers the row written by a later
// version of this code. Dropping the fields this build does not know would
// render a message that is missing something and a digest that says it is
// complete, so the read fails instead.
func TestRenderSnapshotRefusesAFutureSchema(t *testing.T) {
	stored, err := json.Marshal(mustSnapshot(t, fixtureSnapshot()))
	if err != nil {
		t.Fatalf("store the snapshot: %v", err)
	}

	future := string(stored[:len(stored)-1]) + `,"escalation_note":"from a later schema"}`

	var read RenderSnapshot
	if err := json.Unmarshal([]byte(future), &read); err == nil {
		t.Fatal("a snapshot from a later schema was read as if this build understood it")
	}
}

// TestRenderSnapshotDigestCoversEveryField is the mutation test, and it is the
// one that keeps the projection honest: every field here is in the snapshot
// because a message shows it, so every field has to change the digest.
func TestRenderSnapshotDigestCoversEveryField(t *testing.T) {
	baseline := mustDigest(t, mustSnapshot(t, fixtureSnapshot()))
	empty := ""

	mutations := []struct {
		name   string
		change func(*SnapshotInput)
	}{
		{"alert group", func(s *SnapshotInput) { s.AlertGroupID = "ag-0002" }},
		{"revision", func(s *SnapshotInput) { s.Revision = 1 }},
		{"status", func(s *SnapshotInput) { s.Status = GroupAcknowledged }},
		{"title", func(s *SnapshotInput) { s.Title = "disk full" }},
		{"severity", func(s *SnapshotInput) { s.Severity = "warning" }},
		{"team label", func(s *SnapshotInput) { s.TeamLabel = str("billing") }},
		{"team label disappearing", func(s *SnapshotInput) { s.TeamLabel = nil }},
		{"team label empty rather than absent", func(s *SnapshotInput) { s.TeamLabel = &empty }},
		{"team onboarded", func(s *SnapshotInput) { s.TeamOnboarded = false }},
		{"group url", func(s *SnapshotInput) { s.GroupURL = str("https://elsewhere.example") }},
		{"external url", func(s *SnapshotInput) { s.ExternalURL = nil }},
		{"display zone", func(s *SnapshotInput) { s.DisplayTimezone = "Europe/Berlin" }},
		{"acknowledged by", func(s *SnapshotInput) { s.AcknowledgedBy = str("nina") }},
		{"resolved by", func(s *SnapshotInput) { s.ResolvedBy = str("nina") }},
		{"team setup url", func(s *SnapshotInput) { s.TeamSetupURL = nil }},

		{"an alert's status", func(s *SnapshotInput) { s.Alerts[0].Status = AlertResolved }},
		{"an alert's name", func(s *SnapshotInput) { s.Alerts[0].AlertName = "DiskFull" }},
		{"an alert's severity", func(s *SnapshotInput) { s.Alerts[0].Severity = "warning" }},
		{"an alert's mention", func(s *SnapshotInput) { s.Alerts[0].SlackUser = str("U0002") }},
		{"an alert's mention disappearing", func(s *SnapshotInput) { s.Alerts[0].SlackUser = nil }},
		{"an alert's dashboard", func(s *SnapshotInput) { s.Alerts[0].DashboardURL = str("https://dash.example") }},
		{"an alert's runbook", func(s *SnapshotInput) { s.Alerts[0].RunbookURL = str("https://runbook.example") }},
		{"an alert's description", func(s *SnapshotInput) { s.Alerts[0].Description = str("10% left") }},
		{"an alert's start", func(s *SnapshotInput) { s.Alerts[0].StartsAt = fixtureStart.Add(time.Second) }},
		{"an alert's fingerprint", func(s *SnapshotInput) { s.Alerts[0].Fingerprint = "fp-3" }},
		{"an alert disappearing", func(s *SnapshotInput) { s.Alerts = s.Alerts[:1] }},

		{"a line's id", func(s *SnapshotInput) { s.Timeline[0].ID = "e-9" }},
		{"a line's type", func(s *SnapshotInput) { s.Timeline[0].Type = EventStatusChange }},
		{"a line's message", func(s *SnapshotInput) { s.Timeline[0].Message = "something else" }},
		{"a line's actor", func(s *SnapshotInput) { s.Timeline[2].Actor = str("olga") }},
		{"a line's actor disappearing", func(s *SnapshotInput) { s.Timeline[2].Actor = nil }},
		{"a line's time", func(s *SnapshotInput) { s.Timeline[0].CreatedAt = fixtureStart.Add(4 * time.Minute) }},
		{"a line disappearing", func(s *SnapshotInput) { s.Timeline = s.Timeline[1:] }},
		{"the omitted count", func(s *SnapshotInput) { s.TimelineOmitted = 3 }},
		{"a provider's buttons", func(s *SnapshotInput) { s.InteractiveProviders = []string{InteractiveSlack} }},
		{"every provider's buttons", func(s *SnapshotInput) { s.InteractiveProviders = nil }},
	}

	seen := map[string]string{baseline: "baseline"}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			s := fixtureSnapshot()
			m.change(&s)
			got := mustDigest(t, mustSnapshot(t, s))
			if got == baseline {
				t.Fatalf("changing the %s did not change the digest", m.name)
			}
			if other, ok := seen[got]; ok {
				t.Fatalf("changing the %s collided with changing the %s", m.name, other)
			}
			seen[got] = m.name
		})
	}
}

// TestRenderSnapshotRefusesWhatItCannotIdentify pins the refusals. Two of them
// are about the closed enums - a literal from somewhere else would change what
// a digest means without changing the protocol - and one is about the order
// being total: with two alerts sharing a fingerprint, "the same content in a
// different order" would be two different snapshots.
func TestRenderSnapshotRefusesWhatItCannotIdentify(t *testing.T) {
	cases := []struct {
		name   string
		change func(*SnapshotInput)
	}{
		{"no alert group", func(s *SnapshotInput) { s.AlertGroupID = "" }},
		{"a negative revision", func(s *SnapshotInput) { s.Revision = -1 }},
		{"no display zone", func(s *SnapshotInput) { s.DisplayTimezone = "" }},
		{"a group status from somewhere else", func(s *SnapshotInput) { s.Status = GroupStatus("firing") }},
		{"an alert status from somewhere else", func(s *SnapshotInput) { s.Alerts[0].Status = AlertStatus("pending") }},
		{"an alert with no fingerprint", func(s *SnapshotInput) { s.Alerts[0].Fingerprint = "" }},
		{"two alerts with one fingerprint", func(s *SnapshotInput) { s.Alerts[1].Fingerprint = s.Alerts[0].Fingerprint }},
		{"an alert with no start", func(s *SnapshotInput) { s.Alerts[0].StartsAt = time.Time{} }},
		{"the process zone under another name", func(s *SnapshotInput) { s.DisplayTimezone = "Local" }},
		{"a zone nobody has heard of", func(s *SnapshotInput) { s.DisplayTimezone = "Mars/Olympus" }},
		{"a history event type from somewhere else", func(s *SnapshotInput) { s.Timeline[0].Type = TimelineEventType("annotated") }},
		{"a line of history with no id", func(s *SnapshotInput) { s.Timeline[0].ID = "" }},
		{"two lines with one id", func(s *SnapshotInput) { s.Timeline[1].ID = s.Timeline[0].ID }},
		{"a line of history with no time", func(s *SnapshotInput) { s.Timeline[0].CreatedAt = time.Time{} }},
		{"a negative omitted count", func(s *SnapshotInput) { s.TimelineOmitted = -1 }},
		{"buttons on a provider that has none", func(s *SnapshotInput) { s.InteractiveProviders = []string{"webhook"} }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fixtureSnapshot()
			tc.change(&s)

			_, err := NewRenderSnapshot(s)
			if err == nil {
				t.Fatal("the protocol accepted a snapshot it cannot identify")
			}
			if !errors.Is(err, ErrContract) {
				t.Fatalf("expected a contract violation, got: %v", err)
			}
		})
	}
}
