package providers

import (
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

func messageState() keys.SnapshotInput {
	team := "platform"
	return keys.SnapshotInput{
		Title: "Disk filling up", Severity: "critical", TeamLabel: &team,
		Alerts: []keys.AlertSnapshot{{Fingerprint: "fp-1"}, {Fingerprint: "fp-2"}},
	}
}

// TestTheFourNamesAreFilledFromTheAlert. The values go through the
// channel's escaping - here one that shouts, so the test can see it was
// applied - and the template's own words do not.
func TestTheFourNamesAreFilledFromTheAlert(t *testing.T) {
	got := RenderMessage("{{.Title}} is {{.Severity}} for {{.Team}}: {{.AlertsCount}} alert(s)",
		messageState(), strings.ToUpper)
	if want := "DISK FILLING UP is CRITICAL for PLATFORM: 2 alert(s)"; got != want {
		t.Fatalf("the message reads %q, want %q", got, want)
	}

	noTeam := messageState()
	noTeam.TeamLabel = nil
	if got := RenderMessage("team: {{.Team}}.", noTeam, strings.ToUpper); got != "team: ." {
		t.Fatalf("an alert without a team rendered %q", got)
	}
}

// TestWordsWithoutATemplateGoAsWritten, and so does a template nobody can
// render: the API refuses it when the policy is saved, and a row from before
// that check is the policy's words as they are, as every build before this
// one sent them.
func TestWordsWithoutATemplateGoAsWritten(t *testing.T) {
	for _, text := range []string{
		"disk on db-1 is full, please look",
		"{{.Nope}} is not a name the alert has",
		"{{.Title is not closed",
	} {
		if got := RenderMessage(text, messageState(), strings.ToUpper); got != text {
			t.Errorf("%q was rendered as %q", text, got)
		}
	}
}

// TestTheMessageIsBounded. A thousand runes, whether the words are the
// policy's or a title repeated.
func TestTheMessageIsBounded(t *testing.T) {
	long := strings.Repeat("ы", 1200)
	if got := RenderMessage(long, messageState(), strings.ToUpper); len([]rune(got)) != MessageLimit {
		t.Fatalf("a literal of 1200 runes went out as %d", len([]rune(got)))
	}
	state := messageState()
	state.Title = strings.Repeat("Disk ", 100)
	if got := RenderMessage("{{.Title}}{{.Title}}{{.Title}}", state, strings.ToUpper); len([]rune(got)) != MessageLimit {
		t.Fatalf("a template's output of 1500 runes went out as %d", len([]rune(got)))
	}
}

// TestAPolicyIsSavedOnlyWithATemplateThatRenders.
func TestAPolicyIsSavedOnlyWithATemplateThatRenders(t *testing.T) {
	for _, ok := range []string{
		"", "plain words", "{{.Title}} {{.Severity}} {{.Team}} {{.AlertsCount}}", "{{ .Title }} needs {{.Team}}",
	} {
		if err := ValidateMessageTemplate(ok); err != nil {
			t.Errorf("%q was refused: %v", ok, err)
		}
	}
	for text, wants := range map[string]string{
		"{{.Nope}}":      "names something",
		"{{.Title":       "does not parse",
		"{{.Title.Foo}}": "names something",
	} {
		err := ValidateMessageTemplate(text)
		if err == nil || !strings.Contains(err.Error(), wants) {
			t.Errorf("%q was answered %v, want a refusal that says %q", text, err, wants)
		}
	}
}

// TestThePageWordsAreTheFirstReleases. A step without words of its own says
// what the first release said, with the title and the severity escaped for
// the provider, and without the severity when the alert has none.
func TestThePageWordsAreTheFirstReleases(t *testing.T) {
	upper := func(s string) string { return strings.ToUpper(s) }
	state := keys.SnapshotInput{Title: "disk full", Severity: "critical"}
	if got, want := PageWords(state, upper), "You have a new alert: DISK FULL (Severity: CRITICAL)"; got != want {
		t.Fatalf("the words read %q, want %q", got, want)
	}
	if got, want := PageWords(keys.SnapshotInput{Title: "disk full"}, upper), "You have a new alert: DISK FULL"; got != want {
		t.Fatalf("without a severity the words read %q, want %q", got, want)
	}
}
