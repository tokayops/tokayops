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
		x.GeneratorURL == y.GeneratorURL &&
		x.StartsAt.Equal(y.StartsAt) &&
		x.EndsAt.Equal(y.EndsAt) &&
		sameStrings(x.Labels, y.Labels) &&
		sameStrings(x.Annotations, y.Annotations)
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

// AllResolved says the incident is over: nothing it holds is firing.
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
