package api

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/outbound"
)

// The operator's door: a person deciding what a stuck commitment does. One
// command, its outcomes mapped to codes, and the words of the guard that
// refused a decision handed through as they are - the guards live in the
// store, and this door does not repeat them to explain them.

// DecisionRequest is the decision, on the record.
type DecisionRequest struct {
	// Decision is one of assume_accepted, cancel, retry_current_generation,
	// retry_new_generation.
	Decision string `json:"decision"`
	// Reason is required: a decision without one is "somebody decided
	// something". At most 500 characters, counted as characters.
	Reason string `json:"reason"`
	// AcceptedDuplicateRisk is the person saying, on the record, that a second
	// message may exist. Some decisions need it; the refusal says so.
	AcceptedDuplicateRisk bool `json:"accepted_duplicate_risk"`
	// NewExpiresAt is the new deadline a retry of an expired commitment needs.
	// It has to be ahead of the moment it is written.
	NewExpiresAt *time.Time `json:"new_expires_at,omitempty"`
}

// DecisionResponse is what the decision did, or why it was refused.
type DecisionResponse struct {
	Outcome string `json:"outcome"`
	// Status is where the commitment stands after the answer - the new state
	// when the decision applied, the current one when it did not.
	Status string `json:"status,omitempty"`
	// Row is the line of the decision table that applied.
	Row string `json:"row,omitempty"`
	// Detail is the words of the guard that refused the decision.
	Detail string `json:"detail,omitempty"`
	// Delivery is the commitment after the decision applied.
	Delivery *DeliveryDTO `json:"delivery,omitempty"`
}

const decisionReasonLimit = 500

// DecideDelivery godoc
// @Summary Decide what a stuck delivery does
// @Description A person deciding about a commitment in manual_review, permanent_failed or expired. The reason is required (1 to 500 characters). resolved answers 200 with the commitment and the row of the decision table that applied; already_resolved 409 with the current status; invalid_decision 422 with the guard's words in detail; business_closed and recipient_erased 409 with detail; an unknown commitment 404.
// @Tags deliveries
// @Accept json
// @Produce json
// @Param id path string true "Delivery ID"
// @Param body body DecisionRequest true "The decision"
// @Success 200 {object} DecisionResponse
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} DecisionResponse
// @Failure 422 {object} DecisionResponse
// @Router /api/v1/deliveries/{id}/decisions [post]
func (a *API) DecideDelivery(c echo.Context) error {
	id := c.Param("id")
	var req DecisionRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "malformed request body"})
	}
	decision, ok := decisionFrom(req.Decision)
	if !ok {
		return c.JSON(http.StatusBadRequest, ErrorResponse{Error: "unknown decision " + strconv.Quote(req.Decision)})
	}
	reason := strings.TrimSpace(req.Reason)
	if n := utf8.RuneCountInString(reason); n == 0 || n > decisionReasonLimit {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: "reason is required and is at most 500 characters"})
	}
	// The store checks the deadline against the database clock after the lock;
	// this is the same check by the process clock, for a fast answer, and a
	// convenience rather than a guard.
	if req.NewExpiresAt != nil && !req.NewExpiresAt.After(time.Now()) {
		return c.JSON(http.StatusUnprocessableEntity, DecisionResponse{
			Outcome: string(outbound.ResolveInvalidDecision),
			Detail:  "the new deadline is already past",
		})
	}
	userID, _ := c.Get("user_id").(string)
	actor, err := outbound.UserActor(userID)
	if err != nil {
		return c.JSON(http.StatusUnauthorized, ErrorResponse{Error: "no user in the session"})
	}

	result, err := a.store.ResolveAmbiguity(c.Request().Context(), outbound.ResolveAmbiguityRequest{
		IntentID: id, Decision: decision, Actor: actor, Reason: reason,
		AcceptedDuplicateRisk: req.AcceptedDuplicateRisk, NewExpiresAt: req.NewExpiresAt,
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	resp := DecisionResponse{
		Outcome: string(result.Outcome), Status: string(result.Status),
		Row: result.Row, Detail: result.Detail,
	}
	switch result.Outcome {
	case outbound.ResolveResolved:
		// The commitment comes from the transaction that decided, not from a
		// read after it: the decision has applied by now, and nothing that
		// happens after the commit may turn it into a 500.
		if result.Intent != nil {
			delivery := deliveryDTO(*result.Intent)
			resp.Delivery = &delivery
		}
		log.Printf("outbound: user %s decided %s for %s: %s -> %s (%s)",
			userID, decision, id, result.Outcome, result.Status, result.Row)
		return c.JSON(http.StatusOK, resp)
	case outbound.ResolveAlreadyResolved, outbound.ResolveBusinessClosed, outbound.ResolveRecipientErased:
		return c.JSON(http.StatusConflict, resp)
	case outbound.ResolveInvalidDecision:
		return c.JSON(http.StatusUnprocessableEntity, resp)
	case outbound.ResolveNotFound:
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
	default:
		return c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: "unknown outcome " + strconv.Quote(string(result.Outcome))})
	}
}

// decisionFrom reads a decision out of a request: the closed set, or nothing.
func decisionFrom(s string) (outbound.Decision, bool) {
	for _, d := range outbound.Decisions() {
		if string(d) == s {
			return d, true
		}
	}
	return "", false
}
