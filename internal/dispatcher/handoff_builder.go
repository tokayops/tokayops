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
// The reasons are ordered, and the order is part of the contract. A person can
// answer to more than one at once - a link to a channel that was removed, a
// half-finished link to one that is still here - and the reason describes the
// PERSON rather than any one link, so a counter whose label depended on which
// link was walked first would give two instances two series for one skip.
//
// The order is by how much it tells whoever has to act. A provider that is here
// and does not carry a direct message is a configuration somebody can change; a
// provider this build does not know is a channel that was taken away; an empty
// address is a link somebody began; no links at all is a person who never
// began. First match from the top wins.
type skipReason string

const (
	// skipNoDMCapability is a channel that is registered here and does not
	// carry a direct message. Nothing is wrong with the link or the person.
	skipNoDMCapability skipReason = "no_dm_capability"

	// skipUnknownProvider is a link to a provider this build has no channel
	// for - one that was removed, or one that was never a channel at all. Told
	// apart from the above on purpose: the two look identical from the "cannot
	// send" end and mean opposite things to whoever fixes them.
	skipUnknownProvider skipReason = "unknown_provider"

	// skipIdentityIncomplete is a link with no address on it, which is what a
	// link somebody started and never finished looks like.
	skipIdentityIncomplete skipReason = "identity_incomplete"

	// skipNoIdentity is nothing at all: this person has linked no account, so
	// no channel here has any way to reach them.
	skipNoIdentity skipReason = "no_identity"
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
	providers  providerLookup
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

	var recipients []keys.HandoffRecipient
	var left []skipped
	for _, userID := range notify {
		providers := b.reachableThrough(identities[userID])
		if len(providers) == 0 {
			left = append(left, skipped{
				UserID: userID, Reason: b.whyUnreachable(identities[userID]),
			})
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
func (b announcementBuilder) reachableThrough(linked []*model.ExternalIdentity) []string {
	var out []string
	for _, ei := range linked {
		if ei == nil || ei.ExternalID == "" || !b.carriesDM(ei.Provider) {
			continue
		}
		out = append(out, ei.Provider)
	}
	sort.Strings(out)
	return out
}

// whyUnreachable picks the one reason reported for a person nothing can reach,
// by the order the reasons are declared in.
//
// Asked only after reachableThrough has answered with nothing. Four passes
// rather than one, because the answer is about the person and the passes are
// the priority: a link to a channel that was removed outranks a half-finished
// link to one that is still here, whichever order they came back in.
func (b announcementBuilder) whyUnreachable(linked []*model.ExternalIdentity) skipReason {
	for _, ei := range linked {
		if ei != nil && b.registered(ei.Provider) && !b.carriesDM(ei.Provider) {
			return skipNoDMCapability
		}
	}
	for _, ei := range linked {
		if ei != nil && !b.registered(ei.Provider) {
			return skipUnknownProvider
		}
	}
	for _, ei := range linked {
		if ei != nil && ei.ExternalID == "" {
			return skipIdentityIncomplete
		}
	}
	// Everything above has been ruled out, so there is nothing here: a person
	// with a usable link would have been reachable and never asked about.
	return skipNoIdentity
}

func (b announcementBuilder) registered(provider string) bool {
	if b.providers == nil {
		return false
	}
	_, known := b.providers.Capabilities(provider)
	return known
}

func (b announcementBuilder) carriesDM(provider string) bool {
	if b.providers == nil {
		return false
	}
	capabilities, known := b.providers.Capabilities(provider)
	if !known {
		return false
	}
	for _, kind := range capabilities.SupportedTargetKinds {
		if kind == "dm" {
			return true
		}
	}
	return false
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
