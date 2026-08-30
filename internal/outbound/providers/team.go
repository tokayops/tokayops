package providers

// MaxTeamNameLen bounds a team name where a channel writes one into a message.
//
// It lives here, in the package every channel already shares, rather than as a
// number each of them picks. A person linked to two channels gets the same
// announcement twice, and a name cut in one and whole in the other reads as two
// different teams - so the two must agree, and agreement asserted by a test in
// each package is agreement until somebody edits one of them.
//
// Eighty runes is a length, not a limit. What it has to be shorter than is
// whatever the strictest channel takes - Telegram refuses a message over 4096
// characters outright - and an announcement is otherwise a fixed skeleton and
// three formatted instants, so eighty leaves that bound a wide margin even when
// every rune of the name escapes to five characters.
const MaxTeamNameLen = 80
