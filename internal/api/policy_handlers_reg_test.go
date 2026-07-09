package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/labstack/echo/v4"
)

// TestCreatePolicy_EmptySteps_ShouldFail validates that creating a policy
// with no steps is rejected, preventing "zombie jobs".
func TestCreatePolicy_EmptySteps_ShouldFail(t *testing.T) {
	e := echo.New()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil) // Config/Logger nil for now if not needed

	// Setup: Admin user
	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	s.CreateUser(admin)

	// Payroll
	reqBody := PolicyRequest{
		Name:  "Empty Policy",
		Steps: []PolicyStepRequest{}, // EMPTY!
	}
	body, _ := json.Marshal(reqBody)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/policies", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	// Mock auth context
	c.Set("user_id", admin.ID)

	// Execute
	if err := api.CreatePolicy(c); err != nil {
		t.Fatalf("Handler error: %v", err)
	}

	// EXPECTATION: Should fail with 400 Bad Request
	if rec.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 Bad Request for empty steps, got %d", rec.Code)
	}
}
