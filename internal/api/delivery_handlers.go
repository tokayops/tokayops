package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// DeliveryListResponse represents a paginated list of outbox deliveries.
type DeliveryListResponse struct {
	Deliveries []*model.OutboxDelivery `json:"deliveries"`
	Total      int                     `json:"total"`
	Page       int                     `json:"page"`
	TotalPages int                     `json:"total_pages"`
	HasNext    bool                    `json:"has_next"`
	HasPrev    bool                    `json:"has_prev"`
}

// DeliveryDetailResponse represents a delivery with its attempt history.
type DeliveryDetailResponse struct {
	Delivery *model.OutboxDelivery    `json:"delivery"`
	Attempts []*model.DeliveryAttempt `json:"attempts"`
}

// ReplayDeliveryResponse represents the result of a replay request. DeliveryID
// is the commitment the replay stands for - the same one on a repeat with the
// same Idempotency-Key, so the answer to a repeated request is indistinguishable
// from the first.
type ReplayDeliveryResponse struct {
	OK         bool   `json:"ok"`
	Message    string `json:"message"`
	DeliveryID string `json:"delivery_id,omitempty"`
}

// idempotencyKeyLimit bounds the Idempotency-Key header: one to this many bytes.
const idempotencyKeyLimit = 128

// ListIntegrationDeliveries returns paginated deliveries for an integration.
//
// ListIntegrationDeliveries godoc
// @Summary List the deliveries of a webhook integration
// @Description The deliveries made to one generic webhook subscriber, newest first, in the subscriber's own vocabulary (pending, retry, sent, failed). History before the cutover to the delivery domain is not carried. Readable after the integration is deleted.
// @Tags integrations
// @Produce json
// @Param id path string true "Integration ID"
// @Param page query int false "Page (default 1)"
// @Param limit query int false "Page size (default 50, at most 200)"
// @Success 200 {object} DeliveryListResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/integrations/{id}/deliveries [get]
func (a *API) ListIntegrationDeliveries(c echo.Context) error {
	integrationID := c.Param("id")

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
	deliveries, total, err := a.store.ListWebhookDeliveries(c.Request().Context(), integrationID, limit, offset)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	clampedPage, _, totalPages, hasNext, hasPrev := paginationMeta(total, page, limit)

	if deliveries == nil {
		deliveries = []*model.OutboxDelivery{}
	}

	return c.JSON(http.StatusOK, DeliveryListResponse{
		Deliveries: deliveries,
		Total:      total,
		Page:       clampedPage,
		TotalPages: totalPages,
		HasNext:    hasNext,
		HasPrev:    hasPrev,
	})
}

// GetDeliveryDetail returns a delivery with its attempt history. A delivery
// that is not this integration's is not found, whoever asks.
//
// GetDeliveryDetail godoc
// @Summary One delivery of a webhook integration, with its attempts
// @Description The delivery and every attempt made for it. The delivery id is the same id the journal under /deliveries answers about.
// @Tags integrations
// @Produce json
// @Param id path string true "Integration ID"
// @Param deliveryId path string true "Delivery ID"
// @Success 200 {object} DeliveryDetailResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Router /api/v1/integrations/{id}/deliveries/{deliveryId} [get]
func (a *API) GetDeliveryDetail(c echo.Context) error {
	integrationID := c.Param("id")
	deliveryID := c.Param("deliveryId")

	delivery, attempts, err := a.store.WebhookDelivery(c.Request().Context(), integrationID, deliveryID)
	if err != nil {
		if errors.Is(err, store.ErrWebhookDeliveryNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}
	if attempts == nil {
		attempts = []*model.DeliveryAttempt{}
	}

	return c.JSON(http.StatusOK, DeliveryDetailResponse{
		Delivery: delivery,
		Attempts: attempts,
	})
}

// ReplayDelivery delivers the event of a finished delivery to the same
// subscriber again, as a NEW delivery with the subscriber's current address and
// configuration. The Idempotency-Key names the operator's decision: a repeat of
// the request with the same key finds the same new delivery and gets the same
// answer. A delivery still in progress is not replayed - a second live one
// beside it would reach the subscriber twice for certain - and neither is
// anything to a subscriber that is switched off: the switch withdrew its work,
// and a replay would give it back.
//
// ReplayDelivery godoc
// @Summary Deliver a finished delivery's event to the subscriber again
// @Description Creates a NEW delivery of the same event to the same subscriber, at its current address. The Idempotency-Key header is required and names the decision: a repeat with the same key answers with the same delivery. A delivery still in progress is not replayed (409), and neither is one of a disabled integration (409).
// @Tags integrations
// @Produce json
// @Param id path string true "Integration ID"
// @Param deliveryId path string true "Delivery ID"
// @Param Idempotency-Key header string true "1 to 128 bytes"
// @Success 200 {object} ReplayDeliveryResponse
// @Failure 400 {object} ErrorResponse
// @Failure 403 {object} ErrorResponse
// @Failure 404 {object} ErrorResponse
// @Failure 409 {object} ErrorResponse
// @Router /api/v1/integrations/{id}/deliveries/{deliveryId}/replay [post]
func (a *API) ReplayDelivery(c echo.Context) error {
	integrationID := c.Param("id")
	deliveryID := c.Param("deliveryId")

	key := c.Request().Header.Get("Idempotency-Key")
	if key == "" || len(key) > idempotencyKeyLimit {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: "Idempotency-Key header is required and must be 1 to 128 bytes"})
	}
	actor, _ := c.Get("user_id").(string)

	result, err := a.store.ReplayWebhookDelivery(c.Request().Context(), store.WebhookReplayRequest{
		IntegrationID: integrationID, DeliveryID: deliveryID, ClientRequestID: key, Actor: actor,
	})
	switch {
	case errors.Is(err, store.ErrWebhookDeliveryNotFound):
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
	case errors.Is(err, store.ErrWebhookDeliveryNotTerminal):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "delivery is already in progress"})
	case errors.Is(err, store.ErrWebhookSubscriberDisabled):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "integration is disabled; enable it to replay"})
	case errors.Is(err, store.ErrIntegrationBusy):
		return c.JSON(http.StatusConflict, ErrorResponse{Error: "integration is being changed by another request, try again"})
	case err != nil:
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, ReplayDeliveryResponse{
		OK:         true,
		Message:    "delivery queued for replay",
		DeliveryID: result.DeliveryID,
	})
}
