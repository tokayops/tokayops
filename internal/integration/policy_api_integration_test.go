//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/api"
	"github.com/tokayops/tokayops/internal/dispatcher"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
	"github.com/tokayops/tokayops/internal/testutil"
)

// setupPolicyAPITest builds the API with a REAL provider capability registry
// (slack + telegram, both supporting dm + channel), mirroring cmd/tokayops/main.go.
// This exercises the full HTTP -> Store -> DB path for GET /api/v1/providers and
// policy-step validation, which unit tests only cover with a mock registry.
func setupPolicyAPITest(t *testing.T) *APIIntegrationEnv {
	s := testutil.SetupDB(t)

	reg := dispatcher.NewProviderRegistry(s)
	reg.RegisterCapabilities(dispatcher.ProviderCapabilities{
		Name:                 "slack",
		IntegrationType:      model.IntegrationTypeSlack,
		SupportedTargetKinds: []string{"dm", "channel"},
	})
	reg.RegisterCapabilities(dispatcher.ProviderCapabilities{
		Name:                 "telegram",
		IntegrationType:      model.IntegrationTypeTelegram,
		SupportedTargetKinds: []string{"dm", "channel"},
	})

	a := api.NewAPI(s, nil, nil, nil, "", api.NewProviderCapsAdapter(reg))
	wireScheduleServices(a, s)
	e := echo.New()
	a.RegisterRoutes(e)
	return &APIIntegrationEnv{S: s, API: a, Echo: e}
}

// seedGlobalAdmin returns a user with the global admin role, which satisfies
// every RBAC scope (ActionPolicyCreate on global policies needs it).
func seedGlobalAdmin(t *testing.T, s *store.Store, email string) *model.User {
	t.Helper()
	u := testutil.SeedUser(t, s, email)
	u.Role = model.UserRoleAdmin
	if err := s.UpdateUser(u); err != nil {
		t.Fatalf("UpdateUser(admin): %v", err)
	}
	return u
}

func doAPI(t *testing.T, env *APIIntegrationEnv, userID, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := createAuthenticatedRequest(t, method, path, body, userID)
	rec := httptest.NewRecorder()
	env.Echo.ServeHTTP(rec, req)
	return rec
}

func createPolicyViaAPI(t *testing.T, env *APIIntegrationEnv, userID string, body []byte) *model.EscalationPolicy {
	t.Helper()
	rec := doAPI(t, env, userID, http.MethodPost, "/api/v1/policies", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create policy: got %d: %s", rec.Code, rec.Body.String())
	}
	var p model.EscalationPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode created policy: %v", err)
	}
	return &p
}

func getPolicyViaAPI(t *testing.T, env *APIIntegrationEnv, userID, id string) *model.EscalationPolicy {
	t.Helper()
	rec := doAPI(t, env, userID, http.MethodGet, "/api/v1/policies/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get policy %s: got %d: %s", id, rec.Code, rec.Body.String())
	}
	var p model.EscalationPolicy
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	return &p
}

func assertPolicyStep(t *testing.T, s *model.EscalationStep, provider, kind, ttype string) {
	t.Helper()
	if s.Provider != provider || s.TargetKind != kind || s.TargetType != ttype {
		t.Errorf("step: got (provider=%q,target_kind=%q,target_type=%q), want (%q,%q,%q)",
			s.Provider, s.TargetKind, s.TargetType, provider, kind, ttype)
	}
}

// TestPolicyAPI_ListProviders verifies GET /api/v1/providers reflects the real
// dispatcher registry: slack and telegram, each supporting dm + channel.
func TestPolicyAPI_ListProviders(t *testing.T) {
	env := setupPolicyAPITest(t)
	admin := seedGlobalAdmin(t, env.S, "admin@providers.test")

	rec := doAPI(t, env, admin.ID, http.MethodGet, "/api/v1/providers", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /providers: got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Providers []struct {
			Name                 string   `json:"name"`
			SupportedTargetKinds []string `json:"supported_target_kinds"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode providers: %v", err)
	}

	byName := map[string][]string{}
	for _, p := range resp.Providers {
		byName[p.Name] = p.SupportedTargetKinds
	}
	for _, name := range []string{"slack", "telegram"} {
		kinds, ok := byName[name]
		if !ok {
			t.Fatalf("provider %q missing from %+v", name, resp.Providers)
		}
		set := map[string]bool{}
		for _, k := range kinds {
			set[k] = true
		}
		if !set["dm"] || !set["channel"] || len(set) != 2 {
			t.Errorf("expected %s to support [dm, channel], got %v", name, kinds)
		}
	}
}

// TestPolicyAPI_CreateValid_RoundTrips creates a policy with a DM and a channel
// step via the API and verifies (provider, target_kind, target_type) persist and
// round-trip through Store -> DB -> GET.
func TestPolicyAPI_CreateValid_RoundTrips(t *testing.T) {
	env := setupPolicyAPITest(t)
	admin := seedGlobalAdmin(t, env.S, "admin@polvalid.test")

	body := []byte(`{
		"name": "API Valid Policy",
		"steps": [
			{"provider":"slack","target_kind":"dm","target_type":"user","target_id":"U1","timeout_seconds":10,"max_attempts":1},
			{"provider":"slack","target_kind":"channel","target_type":"channel","target_id":"C1","timeout_seconds":10,"max_attempts":1}
		]
	}`)
	created := createPolicyViaAPI(t, env, admin.ID, body)
	if created.ID == "" {
		t.Fatal("expected a generated policy ID")
	}
	if len(created.Steps) != 2 {
		t.Fatalf("expected 2 steps in create response, got %d", len(created.Steps))
	}

	got := getPolicyViaAPI(t, env, admin.ID, created.ID)
	if len(got.Steps) != 2 {
		t.Fatalf("expected 2 steps on GET, got %d", len(got.Steps))
	}
	assertPolicyStep(t, got.Steps[0], "slack", "dm", "user")
	assertPolicyStep(t, got.Steps[1], "slack", "channel", "channel")
}

// TestPolicyAPI_CreateValid_TelegramSteps verifies telegram dm + channel steps
// pass the capability gate and round-trip through Store -> DB -> GET: the proof
// that registering the telegram capability is sufficient.
func TestPolicyAPI_CreateValid_TelegramSteps(t *testing.T) {
	env := setupPolicyAPITest(t)
	admin := seedGlobalAdmin(t, env.S, "admin@poltelegram.test")

	body := []byte(`{
		"name": "Telegram Policy",
		"steps": [
			{"provider":"telegram","target_kind":"dm","target_type":"user","target_id":"U1","timeout_seconds":10,"max_attempts":1},
			{"provider":"telegram","target_kind":"channel","target_type":"channel","target_id":"-100123","timeout_seconds":10,"max_attempts":1}
		]
	}`)
	created := createPolicyViaAPI(t, env, admin.ID, body)
	if len(created.Steps) != 2 {
		t.Fatalf("expected 2 steps in create response, got %d", len(created.Steps))
	}

	got := getPolicyViaAPI(t, env, admin.ID, created.ID)
	if len(got.Steps) != 2 {
		t.Fatalf("expected 2 steps on GET, got %d", len(got.Steps))
	}
	assertPolicyStep(t, got.Steps[0], "telegram", "dm", "user")
	assertPolicyStep(t, got.Steps[1], "telegram", "channel", "channel")
}

// TestPolicyAPI_UpdateValid_RoundTrips verifies PUT keeps the (provider,
// target_kind, target_type) tuple intact.
func TestPolicyAPI_UpdateValid_RoundTrips(t *testing.T) {
	env := setupPolicyAPITest(t)
	admin := seedGlobalAdmin(t, env.S, "admin@polupdate.test")

	created := createPolicyViaAPI(t, env, admin.ID, []byte(`{
		"name": "To Update",
		"steps": [{"provider":"slack","target_kind":"dm","target_type":"user","target_id":"U1","timeout_seconds":10,"max_attempts":1}]
	}`))

	updateBody := []byte(`{
		"name": "Updated",
		"steps": [{"provider":"slack","target_kind":"channel","target_type":"channel","target_id":"C9","timeout_seconds":10,"max_attempts":1}]
	}`)
	rec := doAPI(t, env, admin.ID, http.MethodPut, "/api/v1/policies/"+created.ID, updateBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("update policy: got %d: %s", rec.Code, rec.Body.String())
	}

	got := getPolicyViaAPI(t, env, admin.ID, created.ID)
	if len(got.Steps) != 1 {
		t.Fatalf("expected 1 step after update, got %d", len(got.Steps))
	}
	assertPolicyStep(t, got.Steps[0], "slack", "channel", "channel")
}

// TestPolicyAPI_InvalidStep_Rejected confirms the capability/taxonomy gate
// returns 400 for unknown providers, unsupported kinds, and dm/channel <-> target
// mismatches via the full API path.
func TestPolicyAPI_InvalidStep_Rejected(t *testing.T) {
	env := setupPolicyAPITest(t)
	admin := seedGlobalAdmin(t, env.S, "admin@polinvalid.test")

	cases := []struct {
		name string
		step string
	}{
		{"unknown provider", `{"provider":"pagerduty","target_kind":"dm","target_type":"user","target_id":"U1"}`},
		{"unsupported kind", `{"provider":"slack","target_kind":"sms","target_type":"user","target_id":"U1"}`},
		{"dm + channel target", `{"provider":"slack","target_kind":"dm","target_type":"channel","target_id":"C1"}`},
		{"channel + user target", `{"provider":"slack","target_kind":"channel","target_type":"user","target_id":"U1"}`},
		{"channel + schedule target", `{"provider":"slack","target_kind":"channel","target_type":"schedule","target_id":"S1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"name":"Invalid","steps":[` + tc.step + `]}`)
			rec := doAPI(t, env, admin.ID, http.MethodPost, "/api/v1/policies", body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestPolicyAPI_Duplicate_PreservesProviderKind is the regression guard the doc
// calls out (P0): duplicating a policy must not silently drop or rewrite
// (provider, target_kind). The UI duplicate flow reads a policy then re-creates
// it; this reproduces that exactly and asserts the tuple survives the round-trip.
func TestPolicyAPI_Duplicate_PreservesProviderKind(t *testing.T) {
	env := setupPolicyAPITest(t)
	admin := seedGlobalAdmin(t, env.S, "admin@poldup.test")

	orig := createPolicyViaAPI(t, env, admin.ID, []byte(`{
		"name": "Original",
		"steps": [
			{"provider":"slack","target_kind":"dm","target_type":"user","target_id":"U1","timeout_seconds":10,"max_attempts":1},
			{"provider":"slack","target_kind":"channel","target_type":"channel","target_id":"C1","timeout_seconds":10,"max_attempts":1}
		]
	}`))

	// Re-fetch and rebuild the create request from the fetched steps, exactly like
	// the web UI's duplicate flow (web/js/modules/policies.js openDuplicateModal).
	src := getPolicyViaAPI(t, env, admin.ID, orig.ID)
	dupSteps := make([]map[string]any, len(src.Steps))
	for i, s := range src.Steps {
		dupSteps[i] = map[string]any{
			"provider":        s.Provider,
			"target_kind":     s.TargetKind,
			"target_type":     s.TargetType,
			"target_id":       s.TargetID,
			"timeout_seconds": 10,
			"max_attempts":    1,
		}
	}
	dupBody, err := json.Marshal(map[string]any{"name": "Original (Copy)", "steps": dupSteps})
	if err != nil {
		t.Fatalf("marshal duplicate body: %v", err)
	}

	dup := createPolicyViaAPI(t, env, admin.ID, dupBody)
	if dup.ID == orig.ID {
		t.Fatal("duplicate must be a distinct policy")
	}

	got := getPolicyViaAPI(t, env, admin.ID, dup.ID)
	if len(got.Steps) != 2 {
		t.Fatalf("expected 2 steps in duplicate, got %d", len(got.Steps))
	}
	assertPolicyStep(t, got.Steps[0], "slack", "dm", "user")
	assertPolicyStep(t, got.Steps[1], "slack", "channel", "channel")
}
