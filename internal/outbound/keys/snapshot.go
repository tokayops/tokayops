package keys

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"sort"
	"time"
)

// renderSnapshotProtocol names the hash protocol below. Every protocol in this
// package starts its material with a literal of its own, so a digest of one can
// never collide with a digest of another.
const renderSnapshotProtocol = "render_snapshot/v1"

// RenderSnapshotSchemaV1 is the stored version of the shape below. One number
// covers the stored JSON and the digest, because the codec is defined over the
// same fields: a second counter would eventually disagree with the first.
const RenderSnapshotSchemaV1 = 1

// GroupStatus, AlertStatus and TimelineEventType are closed sets, listed here
// rather than borrowed from the alerting model.
//
// A protocol that says "whatever the model declares today" is not frozen: a
// literal added or renamed somewhere else would silently change what these
// digests mean. A literal outside these lists is a contract violation, not an
// unknown value to pass through.
type GroupStatus string

const (
	GroupNew          GroupStatus = "new"
	GroupProcessing   GroupStatus = "processing"
	GroupTriggered    GroupStatus = "triggered"
	GroupAcknowledged GroupStatus = "acknowledged"
	GroupResolved     GroupStatus = "resolved"
	GroupClosed       GroupStatus = "closed"
)

var groupStatuses = map[GroupStatus]bool{
	GroupNew: true, GroupProcessing: true, GroupTriggered: true,
	GroupAcknowledged: true, GroupResolved: true, GroupClosed: true,
}

type AlertStatus string

const (
	AlertFiring   AlertStatus = "firing"
	AlertResolved AlertStatus = "resolved"
)

var alertStatuses = map[AlertStatus]bool{AlertFiring: true, AlertResolved: true}

type TimelineEventType string

const (
	EventCreated            TimelineEventType = "created"
	EventAlertAdded         TimelineEventType = "alert_added"
	EventAlertResolved      TimelineEventType = "alert_resolved"
	EventAcknowledged       TimelineEventType = "acknowledged"
	EventResolved           TimelineEventType = "resolved"
	EventNotificationSent   TimelineEventType = "notification_sent"
	EventNotificationFailed TimelineEventType = "notification_failed"
	EventNote               TimelineEventType = "note"
	EventStatusChange       TimelineEventType = "status_change"
)

var timelineEventTypes = map[TimelineEventType]bool{
	EventCreated: true, EventAlertAdded: true, EventAlertResolved: true,
	EventAcknowledged: true, EventResolved: true, EventNotificationSent: true,
	EventNotificationFailed: true, EventNote: true, EventStatusChange: true,
}

// AlertSnapshot is one alert as a message shows it.
//
// It is a render projection, not a copy of the alert: labels and annotations
// that never reach a message are deliberately absent, because a field in the
// digest that cannot change a byte of the message would turn an irrelevant
// difference into a conflict between two producers whose cards are identical.
// The full alert stays on the alert group, where audit reads it.
//
// The three URL-ish fields are already resolved: the producer decides whether
// the dashboard came from an annotation or a label, and the renderer is handed
// the answer rather than the rule.
type AlertSnapshot struct {
	Fingerprint  string      `json:"fingerprint"`
	Status       AlertStatus `json:"status"`
	StartsAt     time.Time   `json:"starts_at"`
	AlertName    string      `json:"alert_name"`
	Severity     string      `json:"severity"`
	SlackUser    *string     `json:"slack_user,omitempty"`
	DashboardURL *string     `json:"dashboard_url,omitempty"`
	RunbookURL   *string     `json:"runbook_url,omitempty"`
	Description  *string     `json:"description,omitempty"`
}

func (a AlertSnapshot) encode() ([]byte, error) {
	if a.Fingerprint == "" {
		return nil, contractf("an alert with no fingerprint")
	}
	if !alertStatuses[a.Status] {
		return nil, contractf("alert status %q is not one this protocol knows", a.Status)
	}
	if a.StartsAt.IsZero() {
		return nil, contractf("alert %s has no start time", a.Fingerprint)
	}

	var buf bytes.Buffer
	tagged(&buf, 1, func(b *bytes.Buffer) { encStr(b, a.Fingerprint) })
	tagged(&buf, 2, func(b *bytes.Buffer) { encStr(b, string(a.Status)) })
	tagged(&buf, 3, func(b *bytes.Buffer) { enc(b, int64Bytes(a.StartsAt.UTC().UnixNano())) })
	tagged(&buf, 4, func(b *bytes.Buffer) { encStr(b, a.AlertName) })
	tagged(&buf, 5, func(b *bytes.Buffer) { encStr(b, a.Severity) })
	tagged(&buf, 6, func(b *bytes.Buffer) { encOpt(b, a.SlackUser) })
	tagged(&buf, 7, func(b *bytes.Buffer) { encOpt(b, a.DashboardURL) })
	tagged(&buf, 8, func(b *bytes.Buffer) { encOpt(b, a.RunbookURL) })
	tagged(&buf, 9, func(b *bytes.Buffer) { encOpt(b, a.Description) })
	return buf.Bytes(), nil
}

// TimelineEventSnapshot is one line of the history a card shows.
type TimelineEventSnapshot struct {
	ID        string            `json:"id"`
	Type      TimelineEventType `json:"type"`
	Message   string            `json:"message"`
	Actor     *string           `json:"actor,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

func (e TimelineEventSnapshot) encode() ([]byte, error) {
	if e.ID == "" {
		return nil, contractf("a timeline event with no id")
	}
	if !timelineEventTypes[e.Type] {
		return nil, contractf("timeline event type %q is not one this protocol knows", e.Type)
	}
	if e.CreatedAt.IsZero() {
		return nil, contractf("timeline event %s has no time", e.ID)
	}

	var buf bytes.Buffer
	tagged(&buf, 1, func(b *bytes.Buffer) { encStr(b, e.ID) })
	tagged(&buf, 2, func(b *bytes.Buffer) { encStr(b, string(e.Type)) })
	tagged(&buf, 3, func(b *bytes.Buffer) { encStr(b, e.Message) })
	tagged(&buf, 4, func(b *bytes.Buffer) { encOpt(b, e.Actor) })
	tagged(&buf, 5, func(b *bytes.Buffer) { enc(b, int64Bytes(e.CreatedAt.UTC().UnixNano())) })
	return buf.Bytes(), nil
}

// SnapshotInput is the state a message is rendered from, as a producer supplies
// it: everything a card or a direct message shows, and nothing else.
//
// It exists so that rendering is a function of the commitment rather than of
// the moment: a retry sends what was accepted, and two instances with different
// configuration or different local time zones send the same bytes. Anything a
// renderer would otherwise read live - the base URL, whether interactive
// buttons are configured, whether the team is onboarded, which zone to print
// times in - is frozen here or on the commitment's payload.
//
// It is an input rather than the snapshot itself: what gets stored and hashed
// is the canonical form built from it by NewRenderSnapshot.
type SnapshotInput struct {
	AlertGroupID  string      `json:"alert_group_id"`
	Revision      int64       `json:"revision"`
	Status        GroupStatus `json:"status"`
	Title         string      `json:"title"`
	Severity      string      `json:"severity"`
	TeamLabel     *string     `json:"team_label,omitempty"`
	TeamOnboarded bool        `json:"team_onboarded"`
	GroupURL      *string     `json:"group_url,omitempty"`
	ExternalURL   *string     `json:"external_url,omitempty"`
	// DisplayTimezone is the zone times are printed in. An IANA name, because
	// it has to mean the same thing on every instance: "Local" is whatever the
	// process happens to be set to, which is the very thing this snapshot
	// exists to keep out of a message.
	DisplayTimezone string                  `json:"display_timezone"`
	AcknowledgedBy  *string                 `json:"acknowledged_by,omitempty"`
	ResolvedBy      *string                 `json:"resolved_by,omitempty"`
	Alerts          []AlertSnapshot         `json:"alerts"`
	Timeline        []TimelineEventSnapshot `json:"timeline"`
	TeamSetupURL    *string                 `json:"team_setup_url,omitempty"`
}

// RenderSnapshot is a snapshot that is canonical and valid, and can be nothing
// else.
//
// The type exists because the digest is only as good as the guarantee behind
// it: if a snapshot could be hashed in one order and rendered in another, two
// different messages could share one content identity. So there is one way in -
// NewRenderSnapshot - it settles the order, validates the closed sets, and
// computes the digest once. Reading it back from storage goes through the same
// door, so a row edited by hand fails loudly instead of rendering something its
// key does not describe.
type RenderSnapshot struct {
	content SnapshotInput
	digest  []byte
}

// NewRenderSnapshot canonicalises and validates a snapshot once, at the moment
// it is built.
//
// Ordering is settled here and nowhere else: the stored order, the hashed order
// and the rendered order are the same order. Sorting only before hashing would
// let two snapshots share a digest and still produce different messages, which
// is the one thing a content digest must never allow.
func NewRenderSnapshot(in SnapshotInput) (RenderSnapshot, error) {
	if in.AlertGroupID == "" {
		return RenderSnapshot{}, contractf("a snapshot with no alert group")
	}
	if in.Revision < 0 {
		return RenderSnapshot{}, contractf("revision %d is negative", in.Revision)
	}
	if !groupStatuses[in.Status] {
		return RenderSnapshot{}, contractf("group status %q is not one this protocol knows", in.Status)
	}
	if err := checkDisplayTimezone(in.DisplayTimezone); err != nil {
		return RenderSnapshot{}, err
	}

	// Deep, not shallow. A shallow copy shares every optional value with the
	// caller, and a caller that changes one afterwards would change what gets
	// rendered while the digest went on describing what was accepted - the
	// exact divergence this type exists to make impossible.
	out := in.clone()

	alerts := make(map[string]bool, len(out.Alerts))
	for i := range out.Alerts {
		out.Alerts[i].StartsAt = out.Alerts[i].StartsAt.UTC()
		fingerprint := out.Alerts[i].Fingerprint
		if fingerprint == "" {
			return RenderSnapshot{}, contractf("an alert with no fingerprint")
		}
		if alerts[fingerprint] {
			// Without this the order below is not total, and "the same content
			// in a different input order" would be two different snapshots.
			return RenderSnapshot{}, contractf("alert fingerprint %s appears twice", fingerprint)
		}
		alerts[fingerprint] = true
	}

	events := make(map[string]bool, len(out.Timeline))
	for i := range out.Timeline {
		out.Timeline[i].CreatedAt = out.Timeline[i].CreatedAt.UTC()
		id := out.Timeline[i].ID
		if id == "" {
			return RenderSnapshot{}, contractf("a timeline event with no id")
		}
		if events[id] {
			// Same reason as the alerts above: two events sharing an id leave
			// (time, id) unable to order them, and a stable sort would then
			// carry the input order into the digest.
			return RenderSnapshot{}, contractf("timeline event id %s appears twice", id)
		}
		events[id] = true
	}

	sort.Slice(out.Alerts, func(i, j int) bool {
		if !out.Alerts[i].StartsAt.Equal(out.Alerts[j].StartsAt) {
			return out.Alerts[i].StartsAt.Before(out.Alerts[j].StartsAt)
		}
		return out.Alerts[i].Fingerprint < out.Alerts[j].Fingerprint
	})
	sort.Slice(out.Timeline, func(i, j int) bool {
		if !out.Timeline[i].CreatedAt.Equal(out.Timeline[j].CreatedAt) {
			return out.Timeline[i].CreatedAt.Before(out.Timeline[j].CreatedAt)
		}
		return out.Timeline[i].ID < out.Timeline[j].ID
	})

	digest, err := digestOf(out)
	if err != nil {
		return RenderSnapshot{}, err
	}
	return RenderSnapshot{content: out, digest: digest}, nil
}

// checkDisplayTimezone insists on a zone that means the same thing everywhere.
//
// An empty name is not an alias for UTC, and "Local" is the process zone under
// another spelling - the two instances that must agree about the bytes of a
// message would print different times and hash the same snapshot into different
// cards.
func checkDisplayTimezone(name string) error {
	if name == "" {
		return contractf("a snapshot with no display time zone")
	}
	if name == "Local" {
		return contractf("the display time zone may not be Local: it is whatever " +
			"the process is set to, which is what a snapshot exists to keep out")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return contractf("display time zone %q is not a known IANA name: %v", name, err)
	}
	return nil
}

// Content returns the canonical snapshot for rendering.
//
// Deeply copied, down to every optional value: a renderer that reached back
// into the snapshot it was handed could change what the next attempt sends
// without changing the digest that says what was promised.
func (s RenderSnapshot) Content() SnapshotInput {
	return s.content.clone()
}

// clone copies a snapshot and everything it points at.
func (s SnapshotInput) clone() SnapshotInput {
	out := s
	out.TeamLabel = cloneString(s.TeamLabel)
	out.GroupURL = cloneString(s.GroupURL)
	out.ExternalURL = cloneString(s.ExternalURL)
	out.AcknowledgedBy = cloneString(s.AcknowledgedBy)
	out.ResolvedBy = cloneString(s.ResolvedBy)
	out.TeamSetupURL = cloneString(s.TeamSetupURL)

	out.Alerts = make([]AlertSnapshot, len(s.Alerts))
	for i, a := range s.Alerts {
		a.SlackUser = cloneString(a.SlackUser)
		a.DashboardURL = cloneString(a.DashboardURL)
		a.RunbookURL = cloneString(a.RunbookURL)
		a.Description = cloneString(a.Description)
		out.Alerts[i] = a
	}

	out.Timeline = make([]TimelineEventSnapshot, len(s.Timeline))
	for i, e := range s.Timeline {
		e.Actor = cloneString(e.Actor)
		out.Timeline[i] = e
	}
	return out
}

// cloneString copies an optional value rather than the pointer to it. Sharing
// the pointer would let a caller edit a value this package has already hashed.
func cloneString(v *string) *string {
	if v == nil {
		return nil
	}
	copied := *v
	return &copied
}

// Digest is the canonical digest of the snapshot. It cannot fail and cannot
// disagree with the content: both were settled by the constructor.
func (s RenderSnapshot) Digest() []byte {
	return append([]byte(nil), s.digest...)
}

// MarshalJSON stores the canonical form.
func (s RenderSnapshot) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.content)
}

// UnmarshalJSON reads a stored snapshot back through the same door it went in.
//
// A row that no longer canonicalises - an unknown status, a duplicate id, a
// zone that stopped existing - fails here rather than being rendered into a
// message whose content no longer matches the key it was admitted under.
func (s *RenderSnapshot) UnmarshalJSON(data []byte) error {
	// Unknown fields are refused rather than dropped. A row written by a later
	// schema carries content this build cannot render; swallowing the parts it
	// recognises would produce a message that is missing something and a digest
	// that says it is complete.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var in SnapshotInput
	if err := decoder.Decode(&in); err != nil {
		return err
	}
	restored, err := NewRenderSnapshot(in)
	if err != nil {
		return err
	}
	*s = restored
	return nil
}

// digestOf hashes a canonical snapshot. Unexported because a digest of anything
// else is a number nobody should be able to obtain.
func digestOf(s SnapshotInput) ([]byte, error) {
	var material bytes.Buffer
	encStr(&material, renderSnapshotProtocol)

	tagged(&material, 1, func(b *bytes.Buffer) { encStr(b, s.AlertGroupID) })
	tagged(&material, 2, func(b *bytes.Buffer) { enc(b, int64Bytes(s.Revision)) })
	tagged(&material, 3, func(b *bytes.Buffer) { encStr(b, string(s.Status)) })
	tagged(&material, 4, func(b *bytes.Buffer) { encStr(b, s.Title) })
	tagged(&material, 5, func(b *bytes.Buffer) { encStr(b, s.Severity) })
	tagged(&material, 6, func(b *bytes.Buffer) { encOpt(b, s.TeamLabel) })
	tagged(&material, 7, func(b *bytes.Buffer) { encBool(b, s.TeamOnboarded) })
	tagged(&material, 8, func(b *bytes.Buffer) { encOpt(b, s.GroupURL) })
	tagged(&material, 9, func(b *bytes.Buffer) { encOpt(b, s.ExternalURL) })
	tagged(&material, 10, func(b *bytes.Buffer) { encStr(b, s.DisplayTimezone) })
	tagged(&material, 11, func(b *bytes.Buffer) { encOpt(b, s.AcknowledgedBy) })
	tagged(&material, 12, func(b *bytes.Buffer) { encOpt(b, s.ResolvedBy) })

	alerts := make([][]byte, 0, len(s.Alerts))
	for _, a := range s.Alerts {
		encoded, err := a.encode()
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, encoded)
	}
	tagged(&material, 13, func(b *bytes.Buffer) { encList(b, alerts) })

	events := make([][]byte, 0, len(s.Timeline))
	for _, e := range s.Timeline {
		encoded, err := e.encode()
		if err != nil {
			return nil, err
		}
		events = append(events, encoded)
	}
	tagged(&material, 14, func(b *bytes.Buffer) { encList(b, events) })

	tagged(&material, 15, func(b *bytes.Buffer) { encOpt(b, s.TeamSetupURL) })

	sum := sha256.Sum256(material.Bytes())
	return sum[:], nil
}
