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
//
// Version 2, since 2026-09-05: the history and the buttons came back into the
// snapshot, and the one digest became three. Version 1 rows are still read - by
// exactly two readers, see snapshot_v1.go - but never written.
const renderSnapshotProtocol = "render_snapshot/v2"

// cardDigestProtocol and threadDigestProtocol are the separators of the two
// per-form digests. Their own, and not the snapshot's: a digest over a subset
// of the material would otherwise collide with a slice of the whole.
const (
	cardDigestProtocol   = "render_snapshot/v2/card"
	threadDigestProtocol = "render_snapshot/v2/thread"
)

// RenderSnapshotSchemaV1 is the version this build reads and does not write.
// RenderSnapshotSchemaV2 is the version of the shape below. One number covers
// the stored JSON and the digests, because the codec is defined over the same
// fields: a second counter would eventually disagree with the first.
const (
	RenderSnapshotSchemaV1 = 1
	RenderSnapshotSchemaV2 = 2
)

// GroupStatus and AlertStatus are closed sets, listed here rather than borrowed
// from the alerting model.
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

// TimelineEventType is the closed set of things a line of an alert's history
// can be. Listed here, like the statuses above, rather than borrowed from the
// model: the thread renders an icon per literal, and a literal from somewhere
// else would be a line the protocol cannot name.
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

// The providers whose cards can carry action buttons. A closed set for the
// same reason as the statuses: tag 18 is a list of these literals, and the
// card renderer of each provider looks itself up in it.
const (
	InteractiveSlack    = "slack"
	InteractiveTelegram = "telegram"
)

var interactiveProviders = map[string]bool{InteractiveSlack: true, InteractiveTelegram: true}

// SystemActor is the actor a line of history carries when nobody in particular
// wrote it. The thread prints an actor beside a line when there is one, and
// this literal - like no actor at all - is nobody.
const SystemActor = "system"

// The limits, in runes, on every field that arrives from outside - an alert's
// labels, a person's display name, the text of a note. They are part of
// canonicalisation rather than of rendering, and TimelineLength with them.
//
// The difference matters. Truncating at render time would leave two snapshots
// with the same first N runes and different tails holding different digests
// while producing byte-identical messages - so raising a revision, and sending
// a real edit nobody can see. That is the defect the history was once removed
// from this protocol for; a rule applied after the digest would reintroduce it
// through a field nobody thought of as high-churn.
//
// A value longer than its limit is stored as its first N runes followed by the
// ellipsis. Applying the rule to an already-cut value changes nothing - the
// first N runes of "R[:N]..." are R[:N] - so a stored snapshot canonicalises
// back to itself.
const (
	AlertDescriptionLimit = 120
	AlertNameLimit        = 200
	TitleLimit            = 200
	TimelineMessageLimit  = 300
	ActorLimit            = 100

	// TimelineLength is how many lines of history the snapshot keeps, the
	// most recent ones. What it does not keep is counted, and the count is
	// rendered, so the cut is visible rather than silent.
	TimelineLength = 20
)

// AlertDescriptionEllipsis marks a value the protocol cut.
const AlertDescriptionEllipsis = "..."

// Truncate is the canonical form of a bounded field.
func Truncate(value string, limit int) string {
	r := []rune(value)
	if len(r) <= limit {
		return value
	}
	return string(r[:limit]) + AlertDescriptionEllipsis
}

// TruncateAlertDescription is the canonical form of a description, exported so
// the paths that render a live row rather than an admitted one produce the same
// bytes as the ones that render a snapshot.
func TruncateAlertDescription(description string) string {
	return Truncate(description, AlertDescriptionLimit)
}

func truncateOpt(value *string, limit int) *string {
	if value == nil {
		return nil
	}
	cut := Truncate(*value, limit)
	return &cut
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

// TimelineEventSnapshot is one line of an alert's history as the thread shows
// it: a render projection of a timeline row, like AlertSnapshot is of an alert.
// The metadata of the row never reaches a message and is not here.
type TimelineEventSnapshot struct {
	ID        string            `json:"id"`
	Type      TimelineEventType `json:"type"`
	Message   string            `json:"message"`
	Actor     *string           `json:"actor,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

func (e TimelineEventSnapshot) encode() ([]byte, error) {
	if e.ID == "" {
		return nil, contractf("a line of history with no id")
	}
	if !timelineEventTypes[e.Type] {
		return nil, contractf("history event type %q is not one this protocol knows", e.Type)
	}
	if e.CreatedAt.IsZero() {
		return nil, contractf("history line %s has no time", e.ID)
	}

	var buf bytes.Buffer
	tagged(&buf, 1, func(b *bytes.Buffer) { encStr(b, e.ID) })
	tagged(&buf, 2, func(b *bytes.Buffer) { encStr(b, string(e.Type)) })
	tagged(&buf, 3, func(b *bytes.Buffer) { encStr(b, e.Message) })
	tagged(&buf, 4, func(b *bytes.Buffer) { encOpt(b, e.Actor) })
	tagged(&buf, 5, func(b *bytes.Buffer) { enc(b, int64Bytes(e.CreatedAt.UTC().UnixNano())) })
	return buf.Bytes(), nil
}

// KnownGroupStatus and KnownAlertStatus are the closed sets, asked from
// outside.
//
// A caller that maps its own vocabulary into this one has to be able to tell
// whether a value survived that mapping, or it ends up quietly substituting -
// and a snapshot whose status was silently turned into "processing" is a
// message about a state the alert was never in.
func KnownGroupStatus(status GroupStatus) bool { return groupStatuses[status] }

// KnownAlertStatus reports whether an alert status is one this protocol knows.
func KnownAlertStatus(status AlertStatus) bool { return alertStatuses[status] }

// KnownTimelineEventType reports whether a history event type is one this
// protocol knows.
func KnownTimelineEventType(kind TimelineEventType) bool { return timelineEventTypes[kind] }

// SnapshotInput is the state a message is rendered from, as a producer supplies
// it: everything a card, a thread or a direct message shows, and nothing else.
//
// It exists so that rendering is a function of the commitment rather than of
// the moment: a retry sends what was accepted, and two instances with different
// configuration or different local time zones send the same bytes. Anything a
// renderer would otherwise read live - the base URL, whether interactive
// buttons are configured, whether the team is onboarded, which zone to print
// times in, the history so far - is frozen here.
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
	DisplayTimezone string          `json:"display_timezone"`
	AcknowledgedBy  *string         `json:"acknowledged_by,omitempty"`
	ResolvedBy      *string         `json:"resolved_by,omitempty"`
	Alerts          []AlertSnapshot `json:"alerts"`
	TeamSetupURL    *string         `json:"team_setup_url,omitempty"`

	// Timeline is the history the thread shows: the most recent TimelineLength
	// lines, oldest first, and TimelineOmitted says how many earlier lines were
	// left out. A producer may hand over more than TimelineLength lines and a
	// count of its own; canonicalisation keeps the last ones and adds the rest
	// to the count.
	Timeline        []TimelineEventSnapshot `json:"timeline"`
	TimelineOmitted int64                   `json:"timeline_omitted"`

	// InteractiveProviders names the providers whose cards carry action
	// buttons, as configured at the moment this revision was proposed. The
	// card renderer reads it from here and from nowhere else; an empty list is
	// no buttons anywhere.
	InteractiveProviders []string `json:"interactive_providers"`
}

// RenderSnapshot is a snapshot that is canonical and valid, and can be nothing
// else.
//
// The type exists because the digest is only as good as the guarantee behind
// it: if a snapshot could be hashed in one order and rendered in another, two
// different messages could share one content identity. So there is one way in
// - NewRenderSnapshot - it settles the order, validates the closed sets, and
// computes the digests once. Reading it back from storage goes through the
// same door, so a row edited by hand fails loudly instead of rendering
// something its key does not describe.
//
// A version 1 row comes in through DecodeRenderSnapshotV1 instead, and carries
// only the digest that version had: it can be rendered and rebuilt, and it
// cannot be written back.
type RenderSnapshot struct {
	content SnapshotInput
	schema  int

	digest       []byte
	cardDigest   []byte
	threadDigest []byte
}

// NewRenderSnapshot canonicalises and validates a snapshot once, at the moment
// it is built.
//
// Ordering is settled here and nowhere else: the stored order, the hashed order
// and the rendered order are the same order. Sorting only before hashing would
// let two snapshots share a digest and still produce different messages, which
// is the one thing a content digest must never allow.
func NewRenderSnapshot(in SnapshotInput) (RenderSnapshot, error) {
	if err := checkIdentity(in); err != nil {
		return RenderSnapshot{}, err
	}

	// Deep, not shallow. A shallow copy shares every optional value with the
	// caller, and a caller that changes one afterwards would change what gets
	// rendered while the digest went on describing what was accepted - the
	// exact divergence this type exists to make impossible.
	out := in.clone()

	out.Title = Truncate(out.Title, TitleLimit)
	out.AcknowledgedBy = truncateOpt(out.AcknowledgedBy, ActorLimit)
	out.ResolvedBy = truncateOpt(out.ResolvedBy, ActorLimit)

	if err := canonicalAlerts(out.Alerts, true); err != nil {
		return RenderSnapshot{}, err
	}
	timeline, omitted, err := canonicalTimeline(out.Timeline, out.TimelineOmitted)
	if err != nil {
		return RenderSnapshot{}, err
	}
	out.Timeline, out.TimelineOmitted = timeline, omitted
	if out.InteractiveProviders, err = canonicalProviders(out.InteractiveProviders); err != nil {
		return RenderSnapshot{}, err
	}

	fields, err := encodeFields(out)
	if err != nil {
		return RenderSnapshot{}, err
	}
	return RenderSnapshot{
		content:      out,
		schema:       RenderSnapshotSchemaV2,
		digest:       fields.digest(renderSnapshotProtocol, everyTag),
		cardDigest:   fields.digest(cardDigestProtocol, cardTag),
		threadDigest: fields.digest(threadDigestProtocol, threadTag),
	}, nil
}

// checkIdentity is the part of validation both versions share: the fields
// without which the snapshot is about nothing.
func checkIdentity(in SnapshotInput) error {
	if in.AlertGroupID == "" {
		return contractf("a snapshot with no alert group")
	}
	if in.Revision < 0 {
		return contractf("revision %d is negative", in.Revision)
	}
	if !groupStatuses[in.Status] {
		return contractf("group status %q is not one this protocol knows", in.Status)
	}
	return checkDisplayTimezone(in.DisplayTimezone)
}

// canonicalAlerts settles the alerts in place: instants in UTC, bounded
// fields cut, fingerprints unique, and the order total. The name is cut only
// by version 2 - version 1 stored it whole, and its reader has to reproduce
// its bytes.
func canonicalAlerts(alerts []AlertSnapshot, cutNames bool) error {
	seen := make(map[string]bool, len(alerts))
	for i := range alerts {
		alerts[i].StartsAt = alerts[i].StartsAt.UTC()
		alerts[i].Description = truncateOpt(alerts[i].Description, AlertDescriptionLimit)
		if cutNames {
			alerts[i].AlertName = Truncate(alerts[i].AlertName, AlertNameLimit)
		}
		fingerprint := alerts[i].Fingerprint
		if fingerprint == "" {
			return contractf("an alert with no fingerprint")
		}
		if seen[fingerprint] {
			// Without this the order below is not total, and "the same content
			// in a different input order" would be two different snapshots.
			return contractf("alert fingerprint %s appears twice", fingerprint)
		}
		seen[fingerprint] = true
	}

	sort.Slice(alerts, func(i, j int) bool {
		if !alerts[i].StartsAt.Equal(alerts[j].StartsAt) {
			return alerts[i].StartsAt.Before(alerts[j].StartsAt)
		}
		return alerts[i].Fingerprint < alerts[j].Fingerprint
	})
	return nil
}

// canonicalTimeline settles the history: every line named and of a known
// type, instants in UTC, the message and the actor bounded, nobody as no
// actor, the order total by (time, id), and only the most recent
// TimelineLength lines kept - what is dropped is added to the count.
func canonicalTimeline(lines []TimelineEventSnapshot, omitted int64) ([]TimelineEventSnapshot, int64, error) {
	if omitted < 0 {
		return nil, 0, contractf("%d omitted lines of history is negative", omitted)
	}
	seen := make(map[string]bool, len(lines))
	for i := range lines {
		if lines[i].ID == "" {
			return nil, 0, contractf("a line of history with no id")
		}
		if !timelineEventTypes[lines[i].Type] {
			return nil, 0, contractf("history event type %q is not one this protocol knows", lines[i].Type)
		}
		if lines[i].CreatedAt.IsZero() {
			return nil, 0, contractf("history line %s has no time", lines[i].ID)
		}
		if seen[lines[i].ID] {
			return nil, 0, contractf("history line %s appears twice", lines[i].ID)
		}
		seen[lines[i].ID] = true

		lines[i].CreatedAt = lines[i].CreatedAt.UTC()
		lines[i].Message = Truncate(lines[i].Message, TimelineMessageLimit)
		if a := lines[i].Actor; a != nil && (*a == "" || *a == SystemActor) {
			lines[i].Actor = nil
		}
		lines[i].Actor = truncateOpt(lines[i].Actor, ActorLimit)
	}

	sort.Slice(lines, func(i, j int) bool {
		if !lines[i].CreatedAt.Equal(lines[j].CreatedAt) {
			return lines[i].CreatedAt.Before(lines[j].CreatedAt)
		}
		return lines[i].ID < lines[j].ID
	})

	if extra := len(lines) - TimelineLength; extra > 0 {
		omitted += int64(extra)
		lines = lines[extra:]
	}
	return lines, omitted, nil
}

// canonicalProviders settles the button list: known literals, each once,
// sorted, so the order a producer listed them in is not part of the identity.
func canonicalProviders(providers []string) ([]string, error) {
	seen := make(map[string]bool, len(providers))
	out := make([]string, 0, len(providers))
	for _, p := range providers {
		if !interactiveProviders[p] {
			return nil, contractf("provider %q is not one that carries buttons", p)
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
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

// clone copies a snapshot and everything it points at. The slices come back
// non-nil, so a snapshot with no history stores "timeline": [] rather than
// null - the stored shape has one spelling.
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

	out.InteractiveProviders = make([]string, len(s.InteractiveProviders))
	copy(out.InteractiveProviders, s.InteractiveProviders)

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

// SchemaVersion is the version the snapshot is in: 2 for anything built here,
// 1 for a row read by DecodeRenderSnapshotV1.
func (s RenderSnapshot) SchemaVersion() int { return s.schema }

// Digest is the canonical digest of the snapshot - its identity. It cannot
// fail and cannot disagree with the content: both were settled by the
// constructor.
func (s RenderSnapshot) Digest() []byte {
	return append([]byte(nil), s.digest...)
}

// CardDigest is the digest of what a card renders, and ThreadDigest of what a
// thread renders. Raising a revision compares these, not Digest: a field one
// form does not show must not send that form an edit. Both are nil for a
// version 1 snapshot, which had one form.
func (s RenderSnapshot) CardDigest() []byte {
	return append([]byte(nil), s.cardDigest...)
}

// ThreadDigest is the digest of what a thread renders; see CardDigest.
func (s RenderSnapshot) ThreadDigest() []byte {
	return append([]byte(nil), s.threadDigest...)
}

// MarshalJSON stores the canonical form - of a version 2 snapshot only. A
// version 1 snapshot is read and rebuilt, never written back: writing its
// content under the current shape would store a row whose fields were
// canonicalised by another version's rules.
func (s RenderSnapshot) MarshalJSON() ([]byte, error) {
	if s.schema != RenderSnapshotSchemaV2 {
		return nil, contractf("a version %d snapshot is not written; rebuild it", s.schema)
	}
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

// encodedFields is the material of a canonical snapshot, one entry per tag in
// tag order, before any protocol literal is put in front of it. The three
// digests are taken over subsets of it, and the version 1 digest over the
// subset that version had.
type encodedFields []encodedField

type encodedField struct {
	tag  uint32
	body []byte
}

// encodeFields writes every tagged field once. Tag 14 held the alert's history
// and was retired on 2026-08-25; it is not reused, because a number that meant
// one thing and now means another would let two different snapshots hash the
// same. The history came back as tag 16 with its own encoding.
func encodeFields(s SnapshotInput) (encodedFields, error) {
	field := func(write func(*bytes.Buffer)) []byte {
		var buf bytes.Buffer
		write(&buf)
		return buf.Bytes()
	}

	alerts := make([][]byte, 0, len(s.Alerts))
	for _, a := range s.Alerts {
		encoded, err := a.encode()
		if err != nil {
			return nil, err
		}
		alerts = append(alerts, encoded)
	}
	lines := make([][]byte, 0, len(s.Timeline))
	for _, e := range s.Timeline {
		encoded, err := e.encode()
		if err != nil {
			return nil, err
		}
		lines = append(lines, encoded)
	}
	providers := make([][]byte, 0, len(s.InteractiveProviders))
	for _, p := range s.InteractiveProviders {
		providers = append(providers, []byte(p))
	}

	return encodedFields{
		{1, field(func(b *bytes.Buffer) { encStr(b, s.AlertGroupID) })},
		{2, field(func(b *bytes.Buffer) { enc(b, int64Bytes(s.Revision)) })},
		{3, field(func(b *bytes.Buffer) { encStr(b, string(s.Status)) })},
		{4, field(func(b *bytes.Buffer) { encStr(b, s.Title) })},
		{5, field(func(b *bytes.Buffer) { encStr(b, s.Severity) })},
		{6, field(func(b *bytes.Buffer) { encOpt(b, s.TeamLabel) })},
		{7, field(func(b *bytes.Buffer) { encBool(b, s.TeamOnboarded) })},
		{8, field(func(b *bytes.Buffer) { encOpt(b, s.GroupURL) })},
		{9, field(func(b *bytes.Buffer) { encOpt(b, s.ExternalURL) })},
		{10, field(func(b *bytes.Buffer) { encStr(b, s.DisplayTimezone) })},
		{11, field(func(b *bytes.Buffer) { encOpt(b, s.AcknowledgedBy) })},
		{12, field(func(b *bytes.Buffer) { encOpt(b, s.ResolvedBy) })},
		{13, field(func(b *bytes.Buffer) { encList(b, alerts) })},
		{15, field(func(b *bytes.Buffer) { encOpt(b, s.TeamSetupURL) })},
		{16, field(func(b *bytes.Buffer) { encList(b, lines) })},
		{17, field(func(b *bytes.Buffer) { enc(b, int64Bytes(s.TimelineOmitted)) })},
		{18, field(func(b *bytes.Buffer) { encList(b, providers) })},
	}, nil
}

// digest hashes the protocol literal and the chosen tags, in tag order.
func (f encodedFields) digest(protocol string, keep func(tag uint32) bool) []byte {
	var material bytes.Buffer
	encStr(&material, protocol)
	for _, field := range f {
		if !keep(field.tag) {
			continue
		}
		tagged(&material, field.tag, func(b *bytes.Buffer) { b.Write(field.body) })
	}
	sum := sha256.Sum256(material.Bytes())
	return sum[:]
}

// Which tags each digest sees. The snapshot digest sees everything; the card
// sees everything a card shows, which is everything but the history; the
// thread sees the revision, the zone, the alerts and the history. The revision
// is in both forms for the same reason it was in version 1: a candidate is
// compared at the current number, so the comparison stays about content.
func everyTag(uint32) bool { return true }

func cardTag(tag uint32) bool { return tag <= 15 || tag == 18 }

func threadTag(tag uint32) bool {
	switch tag {
	case 2, 10, 13, 16, 17:
		return true
	}
	return false
}
