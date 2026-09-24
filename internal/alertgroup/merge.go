package alertgroup

import (
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/tokayops/tokayops/internal/model"
)

// Merging what Alertmanager sends into the incident that is open.
//
// These are pure functions on purpose. The decision they add up to - is this a
// merge or a resolution - has to be made under the lock on the incident, from
// the alerts it holds at that moment, and the only way to have it there is for
// the arithmetic to be callable from inside the transaction.
//
// It used to live in the HTTP layer, where the decision was made from a read
// taken before the lock. Two webhooks could then disagree about the same
// incident: one merged a new alert in while the other, holding a set it had
// read a moment earlier, resolved the group without it. Whichever committed
// second decided, and a firing alert was either lost or written into an
// incident that was already over.

// Notification is one thing Alertmanager sent about a group.
//
// Snapshot says the payload is the WHOLE set of alerts Alertmanager still
// reports for this group: it named the group, nothing was cut off by
// max_alerts, and it carried at least one alert. Only then does the absence of
// an alert from it mean anything - in a payload that is not a snapshot, an
// alert can be missing because a list was cut short or because the payload was
// never about a group at all.
type Notification struct {
	Alerts   []model.Alert
	Snapshot bool

	// IntegrationID names the integration this payload came through, so that a
	// change to what that integration declares can reach the groups it feeds -
	// including a group that will never be sent about again.
	IntegrationID string

	// StaleAfterSeconds is what the integration this payload came through says
	// about silence: how long a group may say nothing before the view calls it
	// stale. It is about the sender rather than about the alerts, and it rides
	// with the payload because that is when it is read - fresh, per payload,
	// so an operator's change reaches every instance at once. Zero is nothing
	// declared.
	StaleAfterSeconds int
}

// MergeOutcome is what an Alertmanager payload did to the incident it named.
type MergeOutcome string

const (
	// MergeNoActive: no incident is open for this alert. A firing payload
	// starts the next one; a payload of resolutions refers to an incident that
	// is already over, and there is nothing left to record it against.
	MergeNoActive MergeOutcome = "no_active"

	// MergeIgnored: nothing in the payload belongs to this incident.
	MergeIgnored MergeOutcome = "ignored"

	// MergeUnchanged: the incident already says exactly this. Alertmanager
	// repeats a payload for as long as an alert fires, and a repeat is not news.
	MergeUnchanged MergeOutcome = "unchanged"

	// MergeMerged: the incident now holds alerts it did not before, or holds
	// them in a different state.
	MergeMerged MergeOutcome = "merged"

	// MergeResolved: nothing in the incident is firing any more, so it ended.
	MergeResolved MergeOutcome = "resolved"
)

// MergeResult is what happened, and to which incident.
type MergeResult struct {
	Outcome      MergeOutcome
	AlertGroupID string
}

// FingerprintsOf indexes the alerts an incident holds by identity, which is
// what says whether an incoming alert is new to it and what it was doing
// before.
func FingerprintsOf(alerts []model.Alert) map[string]model.AlertStatus {
	out := make(map[string]model.AlertStatus, len(alerts))
	for _, a := range alerts {
		out[a.Fingerprint] = a.Status
	}
	return out
}

// FilterMergeable drops incoming alerts that do not belong to the incident.
//
// Alertmanager re-sends alerts it resolved earlier for the same aggregation
// group; those were closed together with a previous incident carrying the same
// alert key, so only a FIRING alert may introduce a fingerprint this one has
// never seen. It mirrors the firing-only filter the create path applies.
func FilterMergeable(incoming []model.Alert,
	existing map[string]model.AlertStatus) []model.Alert {

	var relevant []model.Alert
	for _, a := range incoming {
		if _, known := existing[a.Fingerprint]; !known && a.Status != model.AlertStatusFiring {
			continue
		}
		relevant = append(relevant, a)
	}
	return relevant
}

// MergeAlerts is the incident's alerts with the incoming ones applied: the
// latest state of each fingerprint wins.
//
// The result is ordered rather than left in whatever order a map produced. It
// is the order a message lists them in - by when they started - so the stored
// set, the snapshot and the card agree, and two identical payloads produce
// identical rows instead of the same set shuffled.
func MergeAlerts(existing, incoming []model.Alert) []model.Alert {
	state := make(map[string]model.Alert, len(existing)+len(incoming))
	for _, a := range existing {
		state[a.Fingerprint] = a
	}
	for _, a := range incoming {
		state[a.Fingerprint] = a
	}

	out := make([]model.Alert, 0, len(state))
	for _, a := range state {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartsAt.Equal(out[j].StartsAt) {
			return out[i].StartsAt.Before(out[j].StartsAt)
		}
		return out[i].Fingerprint < out[j].Fingerprint
	})
	return out
}

// SameAlerts reports whether two ordered sets are the same STORED alerts, down
// to every field.
//
// It is used to tell a repeat from news, and the comparison is deliberately
// wider than "what a message would show". A description, an alert name, a
// severity, a dashboard link, the label a mention is drawn from - all of those
// change a card and none of them is a fingerprint or a status. Deciding here
// which fields matter to a message would put that judgement in two places, and
// the second copy would be the one that is wrong.
//
// So this answers the narrow question - is this payload news at all - and
// whether the news reaches a message is decided once, downstream, by the digest
// of the render snapshot. A change that is real but invisible costs one
// comparison there and no revision.
func SameAlerts(a, b []model.Alert) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !sameAlert(a[i], b[i]) {
			return false
		}
	}
	return true
}

func sameAlert(x, y model.Alert) bool {
	return x.Fingerprint == y.Fingerprint &&
		x.Status == y.Status &&
		sameInstant(x.StaleSince, y.StaleSince) &&
		x.GeneratorURL == y.GeneratorURL &&
		x.StartsAt.Equal(y.StartsAt) &&
		x.EndsAt.Equal(y.EndsAt) &&
		sameStrings(x.Labels, y.Labels) &&
		sameStrings(x.Annotations, y.Annotations)
}

// sameInstant compares two optional instants: absent is not zero, and two
// present ones are compared as times rather than as pointers.
func sameInstant(x, y *time.Time) bool {
	if x == nil || y == nil {
		return x == nil && y == nil
	}
	return x.Equal(*y)
}

func sameStrings(x, y map[string]string) bool {
	if len(x) != len(y) {
		return false
	}
	for k, v := range x {
		if other, ok := y[k]; !ok || other != v {
			return false
		}
	}
	return true
}

// Applied is what a notification does to the alerts an incident holds.
//
// The arithmetic is here, once, because the two stores that apply a payload -
// the database and the mock the ingester's tests run against - have to answer
// the same way, and because the answer has to be reachable from inside the
// transaction that holds the incident.
type Applied struct {
	// Alerts is what the incident holds afterwards, and Relevant what of the
	// payload merged into it - the lines the history gains are about those.
	Alerts   []model.Alert
	Relevant []model.Alert

	// Resolving says nothing in the incident is firing any more.
	Resolving bool

	// Marked is how many alerts this payload marked as no longer reported,
	// Back the ones it brought back, and HeldOnlyByStale says the
	// incident has just come to be open only because of alerts nobody reports.
	Marked          int
	Back            []Reported
	HeldOnlyByStale bool
}

// Apply works out what a notification means for an incident: what it holds
// afterwards, and what changed about who is still reported.
//
// Pure, and it does not touch what it is given. `at` is the instant the
// payload was taken at, read from the database after the incident was locked.
func Apply(held []model.Alert, notification Notification, at time.Time) Applied {
	relevant := FilterMergeable(notification.Alerts, FingerprintsOf(held))
	alerts := MergeAlerts(held, relevant)

	// Read before the marks: the merge above is what clears one, so the
	// alerts the incident held a moment ago are the only place a silence that
	// has just ended is still written down.
	back := ReportedAgain(held, notification.Alerts, at)
	heldBefore := HeldOnlyByStale(held)

	marked := 0
	if notification.Snapshot {
		alerts, marked = MarkStale(alerts, ReportedIn(notification.Alerts), at)
	}

	return Applied{
		Alerts:          alerts,
		Relevant:        relevant,
		Resolving:       AllResolved(alerts),
		Marked:          marked,
		Back:            back,
		HeldOnlyByStale: !heldBefore && HeldOnlyByStale(alerts),
	}
}

// ReportedIn is every alert a payload carried, by identity. A payload that is
// a snapshot says the incident's other alerts are no longer reported, so this
// is read against the WHOLE payload and not against the part that merges: an
// alert Alertmanager sent is reported whether or not this incident can take
// it.
func ReportedIn(payload []model.Alert) map[string]bool {
	reported := make(map[string]bool, len(payload))
	for _, a := range payload {
		reported[a.Fingerprint] = true
	}
	return reported
}

// MarkStale is the incident's alerts with the ones Alertmanager has
// stopped reporting marked, and how many marks it added.
//
// Only a FIRING alert can be marked. A resolved one is missing from every
// later notification by design - Alertmanager drops it once it has been sent -
// so its absence says nothing. A mark already standing is not rewritten: it
// says when the silence STARTED, and a repeat of the same snapshot is not a
// second silence.
//
// The result is a new slice; the argument is not touched.
func MarkStale(alerts []model.Alert, reported map[string]bool, at time.Time) ([]model.Alert, int) {
	out := make([]model.Alert, len(alerts))
	copy(out, alerts)

	marked := 0
	for i := range out {
		if out[i].Status != model.AlertStatusFiring || reported[out[i].Fingerprint] {
			continue
		}
		if out[i].StaleSince != nil {
			continue
		}
		since := at
		out[i].StaleSince = &since
		marked++
	}
	return out, marked
}

// Reported is an alert Alertmanager started reporting again: how long it was
// missing, and what it came back as.
type Reported struct {
	Status model.AlertStatus
	Silent time.Duration
}

// ReportedAgain is what a payload says about the alerts this incident had
// stopped hearing about. It is read before the merge, from the alerts the
// incident holds now, because the merge is what clears the mark.
//
// A silence measured as negative is reported as none: the mark and this
// instant are both the database's clock, and a clock that stepped back is not
// a negative silence.
func ReportedAgain(held, payload []model.Alert, at time.Time) []Reported {
	silent := make(map[string]time.Time, len(held))
	for _, a := range held {
		if a.StaleSince != nil {
			silent[a.Fingerprint] = *a.StaleSince
		}
	}

	var back []Reported
	for _, a := range payload {
		since, ok := silent[a.Fingerprint]
		if !ok {
			continue
		}
		gap := at.Sub(since)
		if gap < 0 {
			gap = 0
		}
		back = append(back, Reported{Status: a.Status, Silent: gap})
	}
	return back
}

// HeldOnlyByStale says the incident has nothing firing that Alertmanager
// still reports, and is open only because of alerts it has stopped reporting.
//
// This is the state a resolution that went by the alert set alone would have
// ended, which is what counting it is for. It is an upper bound on that and
// not a count of resolutions a policy would actually make: a policy would ask
// more of the payload than its alerts.
func HeldOnlyByStale(alerts []model.Alert) bool {
	stale := false
	for _, a := range alerts {
		switch a.State() {
		case model.AlertStateFiring:
			return false
		case model.AlertStateStale:
			stale = true
		}
	}
	return stale
}

// AllResolved says the incident is over: nothing it holds is firing.
//
// An alert Alertmanager has stopped reporting is still firing here. Absence is
// not a resolution: it is what a silence looks like, and an incident that
// ended because somebody silenced an alert would take the paging with it.
func AllResolved(alerts []model.Alert) bool {
	for _, a := range alerts {
		if a.Status == model.AlertStatusFiring {
			return false
		}
	}
	return true
}

// MergeTimelineEvents is what the incident's history gains from a payload: an
// alert joining it, one clearing, one firing again.
//
// One line per alert in the payload, compared against what the incident held
// BEFORE it - so a payload naming one fingerprint twice writes two lines. That
// is a payload Alertmanager does not send, and the alternative - carrying a
// running view of the merge in here - would make this answer depend on the
// order the payload happened to arrive in.
//
// The microsecond offsets are what make the order deterministic. Several events
// written in one transaction share a timestamp otherwise, and an order that is
// not total is not an order.
func MergeTimelineEvents(alertGroupID string, incoming []model.Alert,
	existing map[string]model.AlertStatus, baseTime time.Time) []*model.TimelineEvent {

	var events []*model.TimelineEvent
	for _, a := range incoming {
		previous, known := existing[a.Fingerprint]

		var eventType model.TimelineEventType
		var message string
		switch {
		case !known && a.Status == model.AlertStatusFiring:
			eventType = model.TimelineEventAlertAdded
			message = "Alert added: " + a.Labels["alertname"]
		case known && previous == model.AlertStatusFiring && a.Status == model.AlertStatusResolved:
			eventType = model.TimelineEventAlertResolved
			message = "Alert resolved: " + a.Labels["alertname"]
		case known && previous == model.AlertStatusResolved && a.Status == model.AlertStatusFiring:
			eventType = model.TimelineEventAlertAdded
			message = "Alert re-fired: " + a.Labels["alertname"]
		default:
			continue
		}

		events = append(events, &model.TimelineEvent{
			ID:           uuid.New().String(),
			AlertGroupID: alertGroupID,
			Type:         eventType,
			Message:      message,
			Actor:        "system",
			Metadata:     map[string]string{"fingerprint": a.Fingerprint},
			CreatedAt:    baseTime.Add(time.Duration(len(events)+1) * time.Microsecond),
		})
	}
	return events
}
