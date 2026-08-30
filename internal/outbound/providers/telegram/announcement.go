package telegram

import (
	"fmt"
	"html"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
)

// What a shift change looks like in Telegram.
//
// The same facts as the Slack announcement and none of its spelling. There is
// no shared renderer on purpose: a shared one would have to write in whatever
// syntax both agree on, which is the syntax neither of them renders - a
// Telegram user reading `*Backend*` and `:mega:` literally is what that
// arrangement looks like from the receiving end.

// announcement is the message one person gets about coming on duty, and the
// parse mode it has to be sent under.
//
// HTML, like the card: the team name is somebody's free text and it goes inside
// a tag, so it is escaped. Sent as plain text instead, the name would be safe
// and the emphasis would be literal angle brackets.
func announcement(payload keys.HandoffPayloadV1) string {
	at := func(t time.Time) string { return displayed(t, payload.Timezone) }
	team := escapedTeamName(payload.TeamName)

	headline := fmt.Sprintf("📣 You are now on-call for team <b>%s</b>.\n\n", team)
	if payload.Kind == keys.HandoffAddedToActiveShift {
		headline = fmt.Sprintf(
			"➕ You have been added to the on-call shift in progress for team <b>%s</b>.\n\n", team)
	}

	return headline +
		fmt.Sprintf("🕐 Rotation shift started: %s\n", at(payload.GridSlotStart)) +
		fmt.Sprintf("🕐 Your assignment effective from: %s\n", at(payload.AssignmentStart)) +
		fmt.Sprintf("🕓 Assignment ends: %s\n", at(payload.AssignmentEnd)) +
		"\n<i>Assignment end is current as of now and may change if the schedule is modified.</i>"
}

// displayed prints an instant in the zone the announcement was written for.
//
// The zone is a NAME and the conversion happens here rather than at admission,
// because a country that changes its clocks between the promise and the
// delivery changes what the correct local time is - and only the name survives
// that.
//
// The zone always loads by the time this runs: the decoder refuses a payload
// carrying one that does not, and nothing renders an undecoded payload. The arm
// below is not a defence against that but against the alternative to it -
// time.In(nil) panics, and a worker that dies on a delivery is worse than an
// hour printed in the wrong zone.
//
// The zone name is escaped with everything else: it comes from the same payload
// the team name does.
func displayed(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return html.EscapeString(fmt.Sprintf("%s (%s)", t.In(loc).Format("Mon Jan 2, 15:04"), zone))
}

// maxTeamNameLen bounds the one field of an announcement that has no length of
// its own.
//
// Everything else here is this package's own words and three formatted
// instants; the team name is free text, and nothing between the form that
// accepts it and this line says how long it may be. Left whole, a team called
// something long enough makes the message exceed telegramMaxMessageLen, the Bot
// API answers "message is too long", and an announcement that was perfectly
// valid ends as a permanent failure - for every person on that team, every
// shift, until somebody renames it.
//
// Eighty runes is what the Slack card allows a team label, and the same number
// is used here so one team name does not arrive whole in one channel and cut in
// the other.
const maxTeamNameLen = 80

// escapedTeamName makes a team name safe to put inside a tag, and short enough
// that the message it goes into can be sent.
//
// Cut BEFORE escaping, so a cut can never sever an entity and leave "&am"
// behind - and by runes, so it cannot sever a character either.
func escapedTeamName(name string) string {
	if r := []rune(name); len(r) > maxTeamNameLen {
		name = string(r[:maxTeamNameLen]) + "…"
	}
	return html.EscapeString(name)
}
