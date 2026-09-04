package slack

import (
	"fmt"
	"time"

	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/outbound/providers"
)

// What a shift change looks like in Slack.
//
// Drawn here, from the payload, and not handed down as text. A producer that
// composed the sentence would be composing it for every channel at once, and
// the only text that survives that is text nobody's channel renders properly -
// which is what a Telegram user reading `*Backend*` and `:mega:` was looking
// at before this existed.

// announcement is the message one person gets about coming on duty.
//
// Both kinds carry both pairs of boundaries, each labelled for what it is. For
// an ordinary handover the first two lines hold the same instant, and that is
// fine: the coincidence is a fact, not a reason to hide one of them. They
// differ exactly where it matters - the shift began at 11:00 and the stand-in's
// assignment starts at 14:00 - and the two kinds differ in their first line:
// one says you came on call, the other that you joined a shift in progress.
func announcement(payload keys.HandoffPayloadV1) string {
	at := func(t time.Time) string { return displayed(t, payload.Timezone) }

	// The team name is free text and it sits in a code span, which is the same
	// place a card's team label sits and for the same reason: mrkdwn has no
	// escape for the asterisk, so a name containing one cannot be made safe
	// inside bold. Inside a span it is literal, and the sanitizer takes the
	// backtick that would end the span away.
	team := teamName(payload.TeamName)
	headline := fmt.Sprintf(":mega: You are now on-call for team `%s`.\n\n", team)
	if payload.Kind == keys.HandoffAddedToActiveShift {
		headline = fmt.Sprintf(
			":heavy_plus_sign: You have been added to the on-call shift in progress for team `%s`.\n\n",
			team)
	}

	return headline +
		fmt.Sprintf(":clock1: Rotation shift started:         %s\n", at(payload.GridSlotStart)) +
		fmt.Sprintf(":clock1: Your assignment effective from: %s\n", at(payload.AssignmentStart)) +
		fmt.Sprintf(":clock4: Assignment ends:                %s\n", at(payload.AssignmentEnd)) +
		"\n_Assignment end is current as of now and may change if the schedule is modified._"
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
func displayed(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		loc = time.UTC
	}
	return fmt.Sprintf("%s (%s)", t.In(loc).Format("Mon Jan 2, 15:04"), zone)
}

// teamName makes a team name safe to put in an announcement, and short enough
// that every channel writes the same one.
//
// Cut BEFORE escaping, so a cut severs neither a character nor an entity, and
// cut to the length the channels share rather than to this one's own: a person
// on both gets the announcement twice, and a name whole in one and cut in the
// other reads as two teams.
func teamName(name string) string {
	if r := []rune(name); len(r) > providers.MaxTeamNameLen {
		name = string(r[:providers.MaxTeamNameLen]) + "…"
	}
	return labelSanitizer.Replace(name)
}
