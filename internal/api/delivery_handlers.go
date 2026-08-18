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

// ReplayDeliveryResponse represents the result of a replay request.
type ReplayDeliveryResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// ListIntegrationDeliveries returns paginated deliveries for an integration.
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
	deliveries, total, err := a.store.GetDeliveriesByIntegrationID(integrationID, limit, offset)
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

// GetDeliveryDetail returns a delivery with its attempt history.
func (a *API) GetDeliveryDetail(c echo.Context) error {
	integrationID := c.Param("id")
	deliveryID := c.Param("deliveryId")

	delivery, err := a.store.GetOutboxDeliveryByID(deliveryID)
	if err != nil {
		if errors.Is(err, store.ErrOutboxDeliveryNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// IDOR check: delivery must belong to this integration
	if delivery.IntegrationID != integrationID {
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
	}

	attempts, err := a.store.GetDeliveryAttempts(deliveryID)
	if err != nil {
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

// ReplayDelivery resets a terminal delivery back to pending for the worker to re-deliver.
func (a *API) ReplayDelivery(c echo.Context) error {
	integrationID := c.Param("id")
	deliveryID := c.Param("deliveryId")

	delivery, err := a.store.GetOutboxDeliveryByID(deliveryID)
	if err != nil {
		if errors.Is(err, store.ErrOutboxDeliveryNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	// IDOR check
	if delivery.IntegrationID != integrationID {
		return c.JSON(http.StatusNotFound, ErrorResponse{Error: "delivery not found"})
	}

	// Atomic: reset delivery + re-open parent event (CAS: only terminal deliveries)
	if err := a.store.ReplayOutboxDelivery(deliveryID); err != nil {
		if errors.Is(err, store.ErrOutboxDeliveryNotTerminal) {
			return c.JSON(http.StatusConflict, ErrorResponse{Error: "delivery is already in progress"})
		}
		return c.JSON(http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
	}

	return c.JSON(http.StatusOK, ReplayDeliveryResponse{
		OK:      true,
		Message: "delivery queued for replay",
	})
}
