package api

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

// ProvidersResponse lists the capability descriptors of every notification
// provider this build declares.
type ProvidersResponse struct {
	Providers []ProviderCapability `json:"providers"`
}

// ListProviders godoc
// @Summary List notification providers
// @Description Capability descriptors for the registered notification providers (sorted by name). Used by the policy editor to populate the (provider, target_kind) dropdown without hardcoding step types.
// @Tags providers
// @Produce json
// @Success 200 {object} ProvidersResponse
// @Router /api/v1/providers [get]
func (a *API) ListProviders(c echo.Context) error {
	// Empty list is a valid response (e.g. fresh install with no providers
	// wired in) - let the frontend disable policy editing rather than 500.
	if a.providerCaps == nil {
		return c.JSON(http.StatusOK, ProvidersResponse{Providers: []ProviderCapability{}})
	}
	return c.JSON(http.StatusOK, ProvidersResponse{Providers: a.providerCaps.AllCapabilities()})
}
