package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// TestCreatePolicy_IncompatibleSteps validates that policies with invalid
// step_type + target_type combinations are rejected.
func TestCreatePolicy_IncompatibleSteps(t *testing.T) {
	e := echo.New()
	s := store.NewMockStore()
	api := NewAPI(s, nil, nil, nil, "", nil)

	admin := &model.User{ID: "admin-1", Role: model.UserRoleAdmin}
	s.CreateUser(admin)

	tests := []struct {
		name       string
		provider   string
		targetKind string
		targetType string
		wantStatus int
	}{
		// Invalid combinations
		{"SlackChannel + Schedule", "slack", "channel", "schedule", http.StatusBadRequest},
		{"SlackChannel + User", "slack", "channel", "user", http.StatusBadRequest},
		{"SlackDM + Channel", "slack", "dm", "channel", http.StatusBadRequest},

		// Valid combinations
		{"SlackDM + User", "slack", "dm", "user", http.StatusCreated},
		{"SlackDM + Schedule", "slack", "dm", "schedule", http.StatusCreated},
		{"SlackChannel + Channel", "slack", "channel", "channel", http.StatusCreated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqBody := PolicyRequest{
				Name: "Compatibility Test Policy",
				Steps: []PolicyStepRequest{
					{
						Provider:     tt.provider,
						TargetKind:   tt.targetKind,
						TargetType:   tt.targetType,
						TargetID:     "some-id",
						DelaySeconds: 60,
					},
				},
			}
			body, _ := json.Marshal(reqBody)

			req := httptest.NewRequest(http.MethodPost, "/api/v1/policies", bytes.NewReader(body))
			req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			c.Set("user_id", admin.ID)

			_ = api.CreatePolicy(c)

			if rec.Code != tt.wantStatus {
				t.Errorf("Provider=%s + TargetKind=%s + TargetType=%s: Expected status %d, got %d",
					tt.provider, tt.targetKind, tt.targetType, tt.wantStatus, rec.Code)
			}
		})
	}
}
