package slack

import (
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// The two messages under a card: the thread, which mirrors the alert's history
// and is edited like the card, and the reply that closes it, said once.
//
// Both are drawn from the snapshot and from nothing else, like the card. What
// is different is the text they carry: every field of it arrived from outside
// - an alert's labels, a person's display name, the words of a note - and Slack
// reads mrkdwn in it. A label reading <!channel> would page the channel, and a
// note holding three backticks would close the block the history sits in. So
// every such field is escaped, once, at the one place it is written.

// mrkdwnEscaper neutralises the three characters mrkdwn reads as markup
// boundaries: an entity for each, which Slack prints as the character.
var mrkdwnEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// codeLineEscaper is mrkdwnEscaper for a line inside a code block, where a
// backtick would end the block and a line break would start a new line of it.
var codeLineEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "&lt;", ">", "&gt;", "`", "'", "\r", " ", "\n", " ")

// mrkdwn makes a field from outside safe to put into a message.
func mrkdwn(s string) string { return mrkdwnEscaper.Replace(s) }

// codeLine makes a field from outside safe to put on a line of a code block.
func codeLine(s string) string { return codeLineEscaper.Replace(s) }

// linkable says whether an address that arrived from outside - an alert's
// dashboard or runbook annotation, Alertmanager's own URL - may be the target
// of a mrkdwn link. A link is <url|label>, and a > or a | inside the address
// ends it early: whatever follows is read as markup, and an annotation
// reading https://a|x> <!channel> pages the channel from the title of every
// card. Only an http or https address with nothing mrkdwn can read is
// linked; anything else is not linked at all, which also keeps a javascript:
// address out of a card. Escaping instead would print a broken address, and
// a link that is not one is worth less than no link.
func linkable(raw string) bool {
	if strings.ContainsAny(raw, "<>|") || strings.ContainsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}) {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// maxThreadAlerts bounds the details section, as the card bounds its list.
const maxThreadAlerts = 10

// RenderThread is the message under the card: the alerts in detail, and the
// history of the group as the snapshot holds it.
func RenderThread(state keys.SnapshotInput) string {
	var b strings.Builder
	b.WriteString("📋 *Alert Details*\n")
	if len(state.Alerts) == 0 {
		b.WriteString("• " + mrkdwn(state.Title) + "\n")
	}
	for i, a := range state.Alerts {
		if i == maxThreadAlerts {
			fmt.Fprintf(&b, "_... and %d more alert details_\n", len(state.Alerts)-maxThreadAlerts)
			break
		}
		icon := "🔴"
		if a.Status == keys.AlertResolved {
			icon = "🟢"
		}
		line := icon + " *" + mrkdwn(a.AlertName) + "*"
		if description := providers.AlertDescription(a); description != "" {
			line += ": " + mrkdwn(description)
		}
		b.WriteString(line + "\n")
	}

	b.WriteString("\n📋 *Timeline*\n```\n")
	if state.TimelineOmitted > 0 {
		fmt.Fprintf(&b, "... and %d earlier events\n", state.TimelineOmitted)
	}
	zone := displayZone(state.DisplayTimezone)
	for _, e := range state.Timeline {
		line := fmt.Sprintf("[%s] %s %s", e.CreatedAt.In(zone).Format("15:04:05 MST"),
			eventIcon(e.Type), codeLine(e.Message))
		if e.Actor != nil && *e.Actor != "" {
			line += " (by " + codeLine(*e.Actor) + ")"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("```")
	return b.String()
}

// RenderReply is the message that closes the thread. It says who resolved the
// alert when a person did; a resolution by the system - every alert cleared -
// is announced without a name.
func RenderReply(state keys.SnapshotInput) string {
	if state.ResolvedBy != nil && *state.ResolvedBy != "" && *state.ResolvedBy != keys.SystemActor {
		return "✅ Resolved by " + mrkdwn(*state.ResolvedBy)
	}
	return "✅ Alert Group Resolved"
}

// eventIcon is the tag a line of history carries, one per type the protocol
// knows. The protocol refuses a type outside its list before a snapshot is
// built, so the last case is unreachable and says so rather than guessing.
func eventIcon(kind keys.TimelineEventType) string {
	switch kind {
	case keys.EventCreated:
		return "[NEW]"
	case keys.EventAlertAdded:
		return "[+]"
	case keys.EventAlertResolved:
		return "[-]"
	case keys.EventAcknowledged:
		return "[ACK]"
	case keys.EventResolved:
		return "[RESOLVED]"
	case keys.EventNotificationSent:
		return "[->]"
	case keys.EventNotificationFailed:
		return "[X]"
	case keys.EventNote:
		return "[NOTE]"
	case keys.EventStatusChange:
		return "[~]"
	default:
		return "[?]"
	}
}

// displayZone is the zone the snapshot says times are printed in. The snapshot
// refused anything that is not a known IANA name, so the fallback is for a
// zone database that has since lost the name, and it is UTC rather than the
// process zone.
func displayZone(name string) *time.Location {
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return time.UTC
}

// coordinates splits a receipt's name into the channel and the timestamp Slack
// gave the message, which is how a satellite finds the card it follows.
func coordinates(ref string) (channel, ts string, ok bool) {
	channel, ts, ok = strings.Cut(ref, "/")
	return channel, ts, ok && channel != "" && ts != ""
}
