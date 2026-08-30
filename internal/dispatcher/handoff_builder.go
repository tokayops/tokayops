package dispatcher

import (
	"fmt"
	"sort"
	"time"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/schedulerender"
)

// Turning a detected shift change into an admission.
//
// The detector answers one question - on schedule S an event of kind K
// happened, and these people are new to it - and this turns that answer into
// the promise: one commitment per person per provider that can actually reach
// them, under an occurrence key two instances derive identically from the same
// rows.
//
// Nothing here renders anything. What arrives in a message is drawn from the
// payload by the channel that sends it, so a shift change looks like Slack in
// Slack and like Telegram in Telegram without either spelling living here.

// announcementMaxAge is how long an announcement is still worth making.
//
// A shift change is news, and news goes off. An instance that comes back after
// a long outage should not tell somebody they are coming on call for a shift
// that is nearly over, and a queue that drained slowly should not either. The
// commitment's deadline is the EARLIER of this and the end of the assignment,
// so the announcement can never outlive the thing it announces.
const announcementMaxAge = time.Hour

// skipReason says why one person new to a shift was not announced to.
//
// The reasons are ordered, and the order is part of the contract: a person can
// answer to more than one of them at once, and a counter whose label depends on
// which check happened to run first is a counter nobody can read.
type skipReason string

const (
	// skipUnlinked is nothing at all: this person has no external identity, so
	// no channel here has any way to reach them.
	skipUnlinked skipReason = "unlinked"

	// skipNoAddress is a link to somewhere a message could go, with nothing to
	// address it to. A link that was started and never finished looks exactly
	// like this, which is why it is told apart from having no link: one is
	// somebody halfway through, the other is somebody who never began.
	skipNoAddress skipReason = "no_address"

	// skipNoDMProvider is linked and addressable, to nothing that sends a
	// direct message here - an identity from a provider that was removed, or
	// one that was never a channel at all.
	skipNoDMProvider skipReason = "no_dm_provider"
)

// skipped is one person left out, and the single reason given for it.
type skipped struct {
	UserID string
	Reason skipReason
}

// addressBook is the part of the store the builder reads: where the people
// coming on duty can be reached, per provider.
//
// It is a filter and not a lookup of addresses. The commitment names the
// INTERNAL user id and the channel resolves the address when it prepares the
// attempt, because an identity relinked between admission and delivery has to
// deliver to the person, not to the account they used to have. What this
// answers is the earlier question: is there any point promising at all.
type addressBook interface {
	GetIdentitiesForUsers(userIDs []string) (map[string][]*model.ExternalIdentity, error)
}

// announcementBuilder turns one detected transition into one admission.
type announcementBuilder struct {
	identities addressBook
	providers  dmProviderLookup
}

// build assembles the admission for one shift change.
//
// A failure to READ is an error, and never an empty answer. The difference is
// the whole reason this returns three things rather than two: an occurrence
// admitted with nobody on it is a durable statement that there was nobody to
// tell, the claim over that occurrence is held forever, and the real
// announcement can never be admitted afterwards. Only a lookup that succeeded
// and came back empty is allowed to say that.
func (b announcementBuilder) build(sc schedulerender.ScheduleOnCall, kind string,
	notify []string, next observation, teamName string) (outbound.Batch, []skipped, error) {

	announced, err := announcementKind(kind)
	if err != nil {
		return outbound.Batch{}, nil, err
	}

	identities, err := b.identities.GetIdentitiesForUsers(notify)
	if err != nil {
		return outbound.Batch{}, nil, fmt.Errorf("read where the incoming shift can be reached: %w", err)
	}

	dm := map[string]bool{}
	if b.providers != nil {
		for _, p := range b.providers.ProvidersSupporting("dm") {
			dm[p] = true
		}
	}

	var recipients []keys.HandoffRecipient
	var left []skipped
	for _, userID := range notify {
		providers := reachableThrough(identities[userID], dm)
		if len(providers) == 0 {
			left = append(left, skipped{UserID: userID, Reason: whyUnreachable(identities[userID], dm)})
			continue
		}
		for _, provider := range providers {
			recipients = append(recipients, keys.HandoffRecipient{
				Provider: provider,
				UserID:   userID,
				// Now. An announcement about a shift that has already started
				// has no reason to wait, and nothing later in its life gives it
				// one: it is one message, sent once.
				Timing:          keys.TimingSpec{Kind: keys.TimingRelativeToAdmission},
				CompletionMode:  keys.CompletionOnAcceptance,
				AmbiguityPolicy: keys.PolicyRetry,
			})
		}
	}

	admission, err := keys.HandoffBatch{
		Occurrence:         announcementOccurrence(announced, sc.ScheduleID, next),
		TeamName:           teamName,
		Timezone:           sc.Timezone,
		GridSlotStart:      next.GridSlotStart,
		AssignmentEnd:      next.AssignmentEnd,
		MaxAge:             announcementMaxAge,
		GrammarVersion:     keys.GrammarV1,
		FingerprintVersion: keys.CurrentBatchFingerprintVersion(),
		Recipients:         recipients,
	}.Admit()
	if err != nil {
		return outbound.Batch{}, nil, fmt.Errorf("build the announcement for schedule %s: %w",
			sc.ScheduleID, err)
	}

	return outbound.Batch{
		Admission: admission,
		Context:   outbound.AnnouncingShiftChange(),
		Actor:     "notifier",
	}, left, nil
}

// reachableThrough is the providers that can carry this announcement to one
// person, sorted, so that one composition produces one set of commitment keys
// whatever order the identities came back in.
func reachableThrough(linked []*model.ExternalIdentity, dm map[string]bool) []string {
	var out []string
	for _, ei := range linked {
		if ei == nil || ei.ExternalID == "" || !dm[ei.Provider] {
			continue
		}
		out = append(out, ei.Provider)
	}
	sort.Strings(out)
	return out
}

// whyUnreachable picks the one reason reported for a person nothing can reach.
//
// Asked only after reachableThrough has answered with nothing, and answered in
// a fixed order rather than by whichever check is written first: somebody with
// a half-finished Slack link and a stale identity from a provider that is gone
// answers to two of these, and the counter has to say the same thing about them
// every time.
func whyUnreachable(linked []*model.ExternalIdentity, dm map[string]bool) skipReason {
	if len(linked) == 0 {
		return skipUnlinked
	}
	for _, ei := range linked {
		if ei != nil && ei.ExternalID == "" && dm[ei.Provider] {
			return skipNoAddress
		}
	}
	return skipNoDMProvider
}

// announcementKind maps what the detector saw onto what the grammar names.
//
// A closed mapping with no default: the two spellings are separate vocabularies
// that happen to agree today, and a kind this does not know is a detector that
// learned something the grammar has not. Defaulting would announce it as an
// ordinary handover - the wrong event, under a key that collides with the right
// one.
func announcementKind(kind string) (keys.HandoffKind, error) {
	switch kind {
	case kindHandoff:
		return keys.HandoffShiftChange, nil
	case kindAddedToActiveShift:
		return keys.HandoffAddedToActiveShift, nil
	default:
		return "", fmt.Errorf("%q is not a kind of shift change this build announces", kind)
	}
}

// announcementOccurrence names the event, and it is the same identity the job
// engine derived from the same parts - kind, schedule, composition, the moment
// the assignment took effect, and the revision that put it on duty. Why each
// part is there is written down beside the detector, in occurrenceOf.
func announcementOccurrence(kind keys.HandoffKind, scheduleID string,
	next observation) keys.Occurrence {

	return keys.Occurrence{
		Kind:            kind,
		ScheduleID:      scheduleID,
		Source:          next.Composition.Source,
		GroupID:         next.Composition.GroupID,
		UserIDs:         next.Composition.UserIDs,
		AssignmentStart: next.AssignmentStart,
		RevisionID:      next.RevisionID,
	}
}
