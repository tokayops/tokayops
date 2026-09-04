package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/outbound/keys"
	"github.com/tokayops/tokayops/internal/store"
)

// The delivery journal over HTTP: the same resource the webhook routes call a
// delivery, in the domain's own vocabulary. GET /deliveries is the operational
// log across families; GET /deliveries/{id} is everything the system knows
// about one commitment; GET /alert-groups/{id}/deliveries is one group's
// deliveries, its paging and its events with the claims on them.
//
// What a row shows about "still trying" is derived here from the row and
// nothing else: pending with attempts is "retrying, next at"; sending is "an
// attempt is open"; pending with no attempts and not_before ahead is
// "scheduled for". The projection the webhook routes make of the same rows
// is not changed by this: that one is a subscriber's contract.

// GroupDeliveryDTO is one commitment as the readers of its alert group see
// it: what was promised, to whom, and where it stands. It carries no
// coordinates - no receipt reference, which names an object at the provider
// the way an address does - because the group's deliveries are read under the
// group's own permission, and that is not the administrator's.
type GroupDeliveryDTO struct {
	ID           string `json:"id"`
	BatchID      string `json:"batch_id"`
	AlertGroupID string `json:"alert_group_id,omitempty"`
	Family       string `json:"family"`
	Kind         string `json:"kind"`
	Provider     string `json:"provider"`
	TargetKind   string `json:"target_kind"`
	TargetRef    string `json:"target_ref"`
	Form         string `json:"form"`
	Status       string `json:"status"`

	GenerationNo         int `json:"generation_no"`
	AttemptsInGeneration int `json:"attempts_in_generation"`
	FailureStreak        int `json:"failure_streak"`

	DesiredRevision      int64  `json:"desired_revision"`
	AppliedRevision      *int64 `json:"applied_revision,omitempty"`
	FinalRevisionApplied bool   `json:"final_revision_applied"`

	ReceiptRecorded       bool `json:"receipt_recorded"`
	RecipientErased       bool `json:"recipient_erased"`
	CancellationRequested bool `json:"cancellation_requested"`
	AcceptedDuplicateRisk bool `json:"accepted_duplicate_risk"`

	NotBefore     time.Time  `json:"not_before"`
	NextAttemptAt time.Time  `json:"next_attempt_at"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// DeliveryAttemptDTO is one line of a commitment's attempt journal: a network
// call, or the proven refusal of one. The address and the provider's summary
// are here - this is the administrator's view, and both are what erasure
// removes from the row when the person is erased.
type DeliveryAttemptDTO struct {
	ID              string          `json:"id"`
	AttemptNo       int             `json:"attempt_no"`
	RecordKind      string          `json:"record_kind"`
	GenerationNo    int             `json:"generation_no"`
	AttemptKind     string          `json:"attempt_kind"`
	Operation       string          `json:"operation"`
	Provider        string          `json:"provider"`
	BoundEndpoint   string          `json:"bound_endpoint,omitempty"`
	AppliedRevision *int64          `json:"applied_revision,omitempty"`
	StartedAt       *time.Time      `json:"started_at,omitempty"`
	FinishedAt      *time.Time      `json:"finished_at,omitempty"`
	Outcome         string          `json:"outcome,omitempty"`
	ErrorClass      string          `json:"error_class,omitempty"`
	ProviderStatus  string          `json:"provider_status,omitempty"`
	ResultDetail    string          `json:"result_detail,omitempty"`
	Receipt         json.RawMessage `json:"receipt,omitempty" swaggertype:"object"`
	// ReceiptRecorded and ReceiptRedactedAt are the three states of the
	// object an attempt made: none, present, and removed by an erasure - the
	// last being proof without coordinates, which is not the same as no proof.
	ReceiptRecorded   bool       `json:"receipt_recorded"`
	ReceiptRedactedAt *time.Time `json:"receipt_redacted_at,omitempty"`
	Summary           string     `json:"summary,omitempty"`
	FinishReason      string     `json:"finish_reason,omitempty"`
}

// DeliveryObservationDTO is a result that arrived for an attempt somebody else
// had already closed.
type DeliveryObservationDTO struct {
	AttemptID         string          `json:"attempt_id"`
	Kind              string          `json:"kind"`
	ObservedAt        time.Time       `json:"observed_at"`
	Outcome           string          `json:"outcome,omitempty"`
	ErrorClass        string          `json:"error_class,omitempty"`
	ProviderStatus    string          `json:"provider_status,omitempty"`
	ResultDetail      string          `json:"result_detail,omitempty"`
	AppliedRevision   *int64          `json:"applied_revision,omitempty"`
	Receipt           json.RawMessage `json:"receipt,omitempty" swaggertype:"object"`
	ReceiptRecorded   bool            `json:"receipt_recorded"`
	ReceiptRedactedAt *time.Time      `json:"receipt_redacted_at,omitempty"`
	Summary           string          `json:"summary,omitempty"`
}

// DeliveryEventDTO is one thing that happened to a commitment without a
// network call: its creation, a withdrawal, an expiry, a person's decision.
type DeliveryEventDTO struct {
	Seq    int    `json:"seq"`
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
	Actor  string `json:"actor,omitempty"`
	// ActorKind says what Actor is: the id of a user, the name of a component, or the text of a build before this one.
	ActorKind  string    `json:"actor_kind"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status,omitempty"`
	At         time.Time `json:"at"`
}

// DeliveryGoneResponse is the 404 of the journal: not found, and how long
// delivery history is kept - 0 when it is kept for good.
type DeliveryGoneResponse struct {
	Error         string `json:"error"`
	RetentionDays int    `json:"retention_days"`
}

// DeliveryJournalResponse is everything the system knows about one commitment.
type DeliveryJournalResponse struct {
	Delivery     DeliveryDTO              `json:"delivery"`
	Attempts     []DeliveryAttemptDTO     `json:"attempts"`
	Observations []DeliveryObservationDTO `json:"observations"`
	Events       []DeliveryEventDTO       `json:"events"`
}

// DeliveriesResponse is one page of the operational journal, with the period
// it was read over - the caller's, or the last day by the database's clock.
type DeliveriesResponse struct {
	Deliveries []DeliveryDTO `json:"deliveries"`
	Total      int           `json:"total"`
	Page       int           `json:"page"`
	TotalPages int           `json:"total_pages"`
	HasNext    bool          `json:"has_next"`
	HasPrev    bool          `json:"has_prev"`
	From       *time.Time    `json:"from,omitempty"`
	To         *time.Time    `json:"to,omitempty"`
}

// AlertGroupDeliveriesResponse is one group's deliveries: the paging it owns,
// and its alert events with every claim on them.
type AlertGroupDeliveriesResponse struct {
	Paging []GroupDeliveryDTO   `json:"paging"`
	Events []EventDeliveriesDTO `json:"events"`
}

// EventDeliveriesDTO is one alert event and the claims taken on it. No batches
// is an event the fan-out has not reached; a batch with no deliveries found
// nobody subscribed.
type EventDeliveriesDTO struct {
	EventID   string               `json:"event_id"`
	EventType string               `json:"event_type"`
	Status    string               `json:"status"`
	CreatedAt time.Time            `json:"created_at"`
	Batches   []BatchDeliveriesDTO `json:"batches"`
}

// BatchDeliveriesDTO is one claim on an event - the fan-out's or a replay's -
// and the commitments it made.
type BatchDeliveriesDTO struct {
	BatchID     string             `json:"batch_id"`
	Kind        string             `json:"kind"`
	Outcome     string             `json:"outcome"`
	IntentCount int                `json:"intent_count"`
	AdmittedAt  time.Time          `json:"admitted_at"`
	Deliveries  []GroupDeliveryDTO `json:"deliveries"`
}

// DeliveryDTO is one commitment as the journal shows it to the administrator:
// the group's view and the receipt reference.
type DeliveryDTO struct {
	GroupDeliveryDTO
	ReceiptRef string `json:"receipt_ref,omitempty"`
}

func groupDeliveryDTO(i outbound.Intent) GroupDeliveryDTO {
	return GroupDeliveryDTO{
		ID: i.ID, BatchID: i.BatchID, AlertGroupID: i.AlertGroupID, Family: i.Family, Kind: string(i.KeyKind),
		Provider: i.Provider, TargetKind: string(i.TargetKind), TargetRef: i.TargetRef,
		Form: string(i.Form), Status: string(i.Status),
		GenerationNo: i.GenerationNo, AttemptsInGeneration: i.AttemptsInGeneration,
		FailureStreak:   i.FailureStreak,
		DesiredRevision: i.DesiredRevision, AppliedRevision: i.AppliedRevision,
		FinalRevisionApplied: i.FinalRevisionApplied,
		ReceiptRecorded:      i.HasReceipt, RecipientErased: i.RecipientErased,
		CancellationRequested: i.CancellationRequested, AcceptedDuplicateRisk: i.AcceptedDuplicateRisk,
		NotBefore: i.NotBefore, NextAttemptAt: i.NextAttemptAt, ExpiresAt: i.ExpiresAt,
		CreatedAt: i.CreatedAt, UpdatedAt: i.UpdatedAt,
	}
}

func groupDeliveryDTOs(intents []outbound.Intent) []GroupDeliveryDTO {
	out := make([]GroupDeliveryDTO, 0, len(intents))
	for _, i := range intents {
		out = append(out, groupDeliveryDTO(i))
	}
	return out
}

func deliveryDTO(i outbound.Intent) DeliveryDTO {
	return DeliveryDTO{GroupDeliveryDTO: groupDeliveryDTO(i), ReceiptRef: i.ReceiptRef}
}

func deliveryDTOs(intents []outbound.Intent) []DeliveryDTO {
	out := make([]DeliveryDTO, 0, len(intents))
	for _, i := range intents {
		out = append(out, deliveryDTO(i))
	}
	return out
}

func journalResponse(j *outbound.Journal) DeliveryJournalResponse {
	out := DeliveryJournalResponse{
		Delivery:     deliveryDTO(j.Intent),
		Attempts:     []DeliveryAttemptDTO{},
		Observations: []DeliveryObservationDTO{},
		Events:       []DeliveryEventDTO{},
	}
	for _, a := range j.Attempts {
		out.Attempts = append(out.Attempts, DeliveryAttemptDTO{
			ID: a.ID, AttemptNo: a.AttemptNo, RecordKind: string(a.RecordKind),
			GenerationNo: a.GenerationNo, AttemptKind: string(a.AttemptKind),
			Operation: string(a.Operation), Provider: a.Provider, BoundEndpoint: a.BoundEndpoint,
			AppliedRevision: a.AppliedRevision, StartedAt: a.StartedAt, FinishedAt: a.FinishedAt,
			Outcome: string(a.Outcome), ErrorClass: a.ErrorClass, ProviderStatus: a.ProviderStatus,
			ResultDetail: a.ResultDetail, Receipt: a.Receipt, ReceiptRecorded: a.ReceiptRecorded,
			ReceiptRedactedAt: a.ReceiptRedactedAt, Summary: a.Summary, FinishReason: a.FinishReason,
		})
	}
	for _, o := range j.Observations {
		out.Observations = append(out.Observations, DeliveryObservationDTO{
			AttemptID: o.AttemptID, Kind: o.Kind, ObservedAt: o.ObservedAt,
			Outcome: string(o.Outcome), ErrorClass: o.ErrorClass, ProviderStatus: o.ProviderStatus,
			ResultDetail: o.ProviderResultDetail, AppliedRevision: o.AppliedRevision,
			Receipt: o.Receipt, ReceiptRecorded: o.ReceiptRecorded,
			ReceiptRedactedAt: o.ReceiptRedactedAt, Summary: o.Summary,
		})
	}
	for _, e := range j.Events {
		out.Events = append(out.Events, DeliveryEventDTO{
			Seq: e.Seq, Kind: e.Kind, Reason: e.Reason, Actor: e.Actor, ActorKind: string(e.ActorKind),
			FromStatus: e.FromStatus, ToStatus: e.ToStatus, At: e.At,
		})
	}
	return out
}

// deliveryFilter reads the journal's filters off the query string. Every
// vocabulary is closed and checked here: a family, a status or a target kind
// this build does not know is a 400, not an empty page that reads as "nothing
// happened".
func deliveryFilter(c echo.Context) (store.IntentFilter, string) {
	q := c.QueryParams()
	var f store.IntentFilter

	if family := q.Get("family"); family != "" {
		if !contains(outbound.Families(), family) {
			return f, "unknown family " + strconv.Quote(family)
		}
		f.Family = family
	}
	f.Provider = q.Get("provider")
	for _, raw := range q["status"] {
		for _, one := range strings.Split(raw, ",") {
			one = strings.TrimSpace(one)
			if one == "" {
				continue
			}
			known := false
			for _, status := range outbound.Statuses() {
				if string(status) == one {
					known = true
				}
			}
			if !known {
				return f, "unknown status " + strconv.Quote(one)
			}
			f.Statuses = append(f.Statuses, outbound.Status(one))
		}
	}
	if kind := q.Get("target_kind"); kind != "" {
		known := false
		for _, k := range keys.TargetKinds() {
			if string(k) == kind {
				known = true
			}
		}
		if !known {
			return f, "unknown target_kind " + strconv.Quote(kind)
		}
		f.TargetKind = keys.TargetKind(kind)
	}
	f.TargetRef = q.Get("target_ref")
	f.AlertGroupID = q.Get("alert_group_id")
	f.EventID = q.Get("event_id")

	from, to, problem := period(q.Get("from"), q.Get("to"))
	if problem != "" {
		return f, problem
	}
	f.From, f.To = from, to
	return f, ""
}

// period is the window the journal is read over. Both ends are optional; with
// neither the store reads the last day by the database's clock. With only an
// end, the start is a day before it: a period is what the journal answers
// about, and a bare end would be "everything up to then".
func period(fromRaw, toRaw string) (from, to *time.Time, problem string) {
	if fromRaw != "" {
		t, err := time.Parse(time.RFC3339, fromRaw)
		if err != nil {
			return nil, nil, "from must be RFC 3339"
		}
		from = &t
	}
	if toRaw != "" {
		t, err := time.Parse(time.RFC3339, toRaw)
		if err != nil {
			return nil, nil, "to must be RFC 3339"
		}
		to = &t
	}
	if from != nil && to != nil && to.Before(*from) {
		return nil, nil, "to is before from"
	}
	if from == nil && to != nil {
		start := to.Add(-24 * time.Hour)
		from = &start
	}
	return from, to, ""
}

func contains(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// ListDeliveries godoc
// @Summary The delivery journal
// @Description Commitments of every delivery family admitted in a period, newest first. The period defaults to the last day by the server's clock; with only `to`, it is the day before it. Unknown values of family, status and target_kind are rejected.
// @Tags deliveries
// @Produce json
// @Param family query string false "notification | handoff | webhook"
// @Param provider query string false "slack | telegram | webhook"
// @Param status query string false "Comma-separated statuses: pending, sending, idle, manual_review, succeeded, permanent_failed, expired, canceled"
// @Param target_kind query string false "user | channel | subscriber"
// @Param target_ref query string false "The recipient as this system names it: a user id, a channel id, an integration id"
// @Param alert_group_id query string false "Commitments owned by this alert group (paging)"
// @Param event_id query string false "Commitments of every claim on this alert event, replays included"
// @Param from query string false "RFC 3339"
// @Param to query string false "RFC 3339"
// @Param page query int false "Page (default 1)"
// @Param limit query int false "Page size (default 50, at most 200)"
// @Success 200 {object} DeliveriesResponse
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Router /api/v1/deliveries [get]
func (a *API) ListDeliveries(c echo.Context) error {
	filter, problem := deliveryFilter(c)
	if problem != "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: problem})
	}
	page, _ := strconv.Atoi(c.QueryParam("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(c.QueryParam("limit"))
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	offset := (page - 1) * limit

	intents, total, err := a.store.ListIntents(c.Request().Context(), filter, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	clampedPage, _, totalPages, hasNext, hasPrev := paginationMeta(total, page, limit)
	return c.JSON(http.StatusOK, DeliveriesResponse{
		Deliveries: deliveryDTOs(intents), Total: total, Page: clampedPage,
		TotalPages: totalPages, HasNext: hasNext, HasPrev: hasPrev,
		From: filter.From, To: filter.To,
	})
}

// GetDeliveryJournal godoc
// @Summary The journal of one delivery
// @Description Everything the system knows about one commitment: what it is, every attempt with its outcome, results that arrived late, and what happened to it without a call - withdrawals, expiry, decisions. The id is the same one the webhook routes call a delivery.
// @Tags deliveries
// @Produce json
// @Param id path string true "Delivery ID"
// @Success 200 {object} DeliveryJournalResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} DeliveryGoneResponse
// @Router /api/v1/deliveries/{id} [get]
func (a *API) GetDeliveryJournal(c echo.Context) error {
	journal, err := a.store.IntentJournal(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	if journal == nil {
		// A delivery that is not here was never here, or was here and is past
		// the window. The answer says how long history is kept, so that the
		// person reading it knows which.
		return c.JSON(http.StatusNotFound, DeliveryGoneResponse{
			Error: "delivery not found", RetentionDays: a.deliveryRetentionDays,
		})
	}
	return c.JSON(http.StatusOK, journalResponse(journal))
}

// GetAlertGroupDeliveries godoc
// @Summary The deliveries of an alert group
// @Description The paging commitments the group owns, and its alert events with every claim taken on them - the fan-out's and each replay's - and the webhook deliveries under those. An event with no batches has not been fanned out yet; a batch with no deliveries found nobody subscribed. One snapshot: nothing here is spliced from two moments.
// @Tags alert-groups
// @Produce json
// @Param id path string true "Alert Group ID"
// @Success 200 {object} AlertGroupDeliveriesResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/alert-groups/{id}/deliveries [get]
func (a *API) GetAlertGroupDeliveries(c echo.Context) error {
	deliveries, err := a.store.AlertGroupDeliveries(c.Request().Context(), c.Param("id"))
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	out := AlertGroupDeliveriesResponse{
		Paging: groupDeliveryDTOs(deliveries.Paging),
		Events: []EventDeliveriesDTO{},
	}
	for _, e := range deliveries.Events {
		event := EventDeliveriesDTO{
			EventID: e.EventID, EventType: e.EventType, Status: e.Status, CreatedAt: e.CreatedAt,
			Batches: []BatchDeliveriesDTO{},
		}
		for _, b := range e.Batches {
			event.Batches = append(event.Batches, BatchDeliveriesDTO{
				BatchID: b.BatchID, Kind: string(b.Kind), Outcome: b.Outcome,
				IntentCount: b.IntentCount, AdmittedAt: b.AdmittedAt,
				Deliveries: groupDeliveryDTOs(b.Deliveries),
			})
		}
		out.Events = append(out.Events, event)
	}
	return c.JSON(http.StatusOK, out)
}
