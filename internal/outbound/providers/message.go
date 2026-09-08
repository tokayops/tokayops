package providers

import (
	"fmt"
	"io"
	"strings"
	// A message is plain text or a provider's markup, escaped by the provider; it is not HTML.
	// nosemgrep: go.lang.security.audit.xss.import-text-template.import-text-template
	"text/template"
	"unicode/utf8"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// The words of a direct message. A step's Message is a template over four
// names, and the four are taken from the snapshot the message renders:
// Title, Severity, Team and AlertsCount. The form promised them since the
// first version; this is the build that fills them in.

// MessageFields are the names a message template may use, and nothing else.
type MessageFields struct {
	Title       string
	Severity    string
	Team        string
	AlertsCount int
}

// MessageLimit bounds what a template can produce: a title of a thousand
// runes repeated ten times is not a page anybody reads, and Telegram refuses
// a message past four thousand.
const MessageLimit = 1000

// ValidateMessageTemplate is the check a policy is saved under: the template
// parses, and every name in it is one of the four. Checked here, once, so
// that an attempt never meets a template it cannot render.
func ValidateMessageTemplate(text string) error {
	tmpl, err := template.New("message").Parse(text)
	if err != nil {
		return fmt.Errorf("the message template does not parse: %w", err)
	}
	probe := MessageFields{Title: "title", Severity: "severity", Team: "team", AlertsCount: 1}
	if err := tmpl.Execute(io.Discard, probe); err != nil {
		return fmt.Errorf("the message template names something the alert does not have: %w", err)
	}
	return nil
}

// RenderMessage is the step's words for one alert. The values are the
// snapshot's, passed through the channel's escaping - they came from outside
// and the channel reads markup in them - while the template's own text is
// the policy's and goes as written. A template that does not render - one
// saved before it was checked - goes as written too, which is what every
// build before this one did with it.
func RenderMessage(text string, state keys.SnapshotInput, escape func(string) string) string {
	if !strings.Contains(text, "{{") {
		return truncateRunes(text, MessageLimit)
	}
	tmpl, err := template.New("message").Parse(text)
	if err != nil {
		return truncateRunes(text, MessageLimit)
	}
	fields := MessageFields{
		Title: escape(state.Title), Severity: escape(state.Severity), AlertsCount: len(state.Alerts),
	}
	if state.TeamLabel != nil {
		fields.Team = escape(*state.TeamLabel)
	}
	var out strings.Builder
	if err := tmpl.Execute(&out, fields); err != nil {
		return truncateRunes(text, MessageLimit)
	}
	return truncateRunes(out.String(), MessageLimit)
}

func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit])
}
