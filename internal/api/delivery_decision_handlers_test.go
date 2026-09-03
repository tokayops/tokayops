package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/outbound"
	"github.com/tokayops/tokayops/internal/store"
)

// The operator's door over a store that answers what it is told: what the
// door refuses before asking, how each outcome becomes a code, what it hands
// the store, and who may knock. The decisions themselves are tested against
// Postgres.

type decisionStoreFake struct {
	store.StoreInterface
	requests []outbound.ResolveAmbiguityRequest
	result   outbound.ResolveAmbiguityResult
	// readsAfter counts reads of the commitment the door makes after the
	// decision. The decision has committed by then: a read that fails would
	// answer 500 about a decision that applied, and one that succeeds may
	// describe a later moment. There must be none.
	readsAfter int
}

func (f *decisionStoreFake) ResolveAmbiguity(_ context.Context, req outbound.ResolveAmbiguityRequest) (outbound.ResolveAmbiguityResult, error) {
	f.requests = append(f.requests, req)
	return f.result, nil
}

func (f *decisionStoreFake) IntentJournal(_ context.Context, _ string) (*outbound.Journal, error) {
	f.readsAfter++
	return nil, errors.New("the database went away after the commit")
}

func (f *decisionStoreFake) ListIntents(_ context.Context, _ store.IntentFilter, _, _ int) ([]outbound.Intent, int, error) {
	f.readsAfter++
	return nil, 0, errors.New("the database went away after the commit")
}

// decided is the commitment as a decision leaves it.
func decided(id string, status outbound.Status) *outbound.Intent {
	return &outbound.Intent{ID: id, Status: status, Family: outbound.FamilyNotification, Provider: "slack"}
}

type decisionRoutes struct {
	fake *decisionStoreFake
	e    *echo.Echo
}

func setupDecisionRoutes(t *testing.T) *decisionRoutes {
	t.Helper()
	s := store.NewMockStore()
	if err := s.CreateUser(&model.User{ID: "bob", Email: "bob@test.com", Role: model.UserRoleUser}); err != nil {
		t.Fatal(err)
	}
	fake := &decisionStoreFake{StoreInterface: s,
		result: outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveResolved, Status: outbound.StatusCanceled,
			Row: "T30", Intent: decided("d-1", outbound.StatusCanceled)},
	}
	a := NewAPI(fake, nil, nil, nil, "", nil)
	e := echo.New()
	a.RegisterRoutes(e)
	return &decisionRoutes{fake: fake, e: e}
}

func (r *decisionRoutes) decide(user, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/deliveries/"+id+"/decisions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	addAuth(req, user)
	rec := httptest.NewRecorder()
	r.e.ServeHTTP(rec, req)
	return rec
}

func decodeDecision(t *testing.T, rec *httptest.ResponseRecorder) DecisionResponse {
	t.Helper()
	var resp DecisionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return resp
}

// TestADecisionAnswersByItsOutcome: the table of S5-D3, one outcome per row.
func TestADecisionAnswersByItsOutcome(t *testing.T) {
	r := setupDecisionRoutes(t)
	for _, tt := range []struct {
		result outbound.ResolveAmbiguityResult
		code   int
	}{
		{outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveResolved, Status: outbound.StatusCanceled, Row: "T30",
			Intent: decided("d-1", outbound.StatusCanceled)}, http.StatusOK},
		{outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveAlreadyResolved, Status: outbound.StatusSucceeded}, http.StatusConflict},
		{outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveInvalidDecision, Status: outbound.StatusManualReview,
			Detail: "a new generation after an ambiguous attempt needs the duplicate risk accepted"}, http.StatusUnprocessableEntity},
		{outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveBusinessClosed, Status: outbound.StatusPermanentFailed,
			Detail: "the alert this commitment belongs to is over; nothing is retried for it"}, http.StatusConflict},
		{outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveRecipientErased, Status: outbound.StatusManualReview,
			Detail: "the recipient was erased; the only decision left is cancel"}, http.StatusConflict},
		{outbound.ResolveAmbiguityResult{Outcome: outbound.ResolveNotFound}, http.StatusNotFound},
	} {
		r.fake.result = tt.result
		rec := r.decide("denis", "d-1", `{"decision":"cancel","reason":"nobody is listening"}`)
		if rec.Code != tt.code {
			t.Errorf("%s answered %d, want %d: %s", tt.result.Outcome, rec.Code, tt.code, rec.Body.String())
			continue
		}
		if tt.result.Outcome == outbound.ResolveNotFound {
			continue
		}
		resp := decodeDecision(t, rec)
		if resp.Outcome != string(tt.result.Outcome) || resp.Status != string(tt.result.Status) ||
			resp.Detail != tt.result.Detail || resp.Row != tt.result.Row {
			t.Errorf("%s reads %+v", tt.result.Outcome, resp)
		}
		if tt.result.Outcome == outbound.ResolveResolved {
			if resp.Delivery == nil || resp.Delivery.ID != "d-1" || resp.Delivery.Status != "canceled" {
				t.Errorf("resolved does not carry the commitment: %+v", resp)
			}
		} else if resp.Delivery != nil {
			t.Errorf("%s carries a commitment: %+v", tt.result.Outcome, resp)
		}
	}
	// Nothing was read after a decision: the store that answers every read
	// with an error was never asked, and the decision that applied answered
	// 200 regardless.
	if r.fake.readsAfter != 0 {
		t.Errorf("the door read the commitment %d time(s) after the decision had committed", r.fake.readsAfter)
	}
}

// TestADecisionNeedsAReasonInCharacters: the reason is required, and its
// length is counted in characters - five hundred Cyrillic ones pass, a
// byte count would refuse them as a thousand.
func TestADecisionNeedsAReasonInCharacters(t *testing.T) {
	r := setupDecisionRoutes(t)
	if rec := r.decide("denis", "d-1", `{"decision":"cancel","reason":""}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an empty reason answered %d", rec.Code)
	}
	if rec := r.decide("denis", "d-1", `{"decision":"cancel","reason":"   "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("a blank reason answered %d", rec.Code)
	}
	if len(r.fake.requests) != 0 {
		t.Fatalf("the store was asked %d times about decisions with no reason", len(r.fake.requests))
	}
	fiveHundred := strings.Repeat("ж", 500)
	if rec := r.decide("denis", "d-1", `{"decision":"cancel","reason":"`+fiveHundred+`"}`); rec.Code != http.StatusOK {
		t.Errorf("five hundred characters answered %d: %s", rec.Code, rec.Body.String())
	}
	if len(r.fake.requests) != 1 || r.fake.requests[0].Reason != fiveHundred {
		t.Fatalf("the reason did not reach the store as given")
	}
	if rec := r.decide("denis", "d-1", `{"decision":"cancel","reason":"`+fiveHundred+`ж"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("five hundred and one characters answered %d", rec.Code)
	}
	if len(r.fake.requests) != 1 {
		t.Fatalf("the store was asked about a reason that was too long")
	}
}

// TestADecisionRefusesWhatItDoesNotKnow: an unknown decision, a malformed
// body, a deadline already past - none of them reach the store.
func TestADecisionRefusesWhatItDoesNotKnow(t *testing.T) {
	r := setupDecisionRoutes(t)
	if rec := r.decide("denis", "d-1", `{"decision":"shrug","reason":"why not"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("an unknown decision answered %d", rec.Code)
	}
	if rec := r.decide("denis", "d-1", `{"decision":`); rec.Code != http.StatusBadRequest {
		t.Errorf("a malformed body answered %d", rec.Code)
	}
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	rec := r.decide("denis", "d-1", `{"decision":"retry_current_generation","reason":"again","new_expires_at":"`+past+`"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a deadline in the past answered %d: %s", rec.Code, rec.Body.String())
	} else if resp := decodeDecision(t, rec); resp.Outcome != "invalid_decision" || resp.Detail != "the new deadline is already past" {
		t.Errorf("a deadline in the past reads %+v", resp)
	}
	if len(r.fake.requests) != 0 {
		t.Fatalf("the store was asked %d times about requests that were refused", len(r.fake.requests))
	}
}

// TestTheDecisionCarriesThePerson: what the store is handed is the decision
// as typed, the person from the session, and the flag and deadline as given.
func TestTheDecisionCarriesThePerson(t *testing.T) {
	r := setupDecisionRoutes(t)
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	rec := r.decide("denis", "d-1", `{"decision":"retry_new_generation","reason":"  the channel is back  ",`+
		`"accepted_duplicate_risk":true,"new_expires_at":"`+at.Format(time.RFC3339)+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if len(r.fake.requests) != 1 {
		t.Fatalf("%d requests reached the store", len(r.fake.requests))
	}
	got := r.fake.requests[0]
	if got.IntentID != "d-1" || got.Decision != outbound.DecisionRetryNewGeneration || got.Reason != "the channel is back" ||
		!got.AcceptedDuplicateRisk || got.NewExpiresAt == nil || !got.NewExpiresAt.Equal(at) {
		t.Errorf("the store was handed %+v", got)
	}
	if got.Actor != byUser("denis") {
		t.Errorf("the decision is signed by %s, want the person in the session", got.Actor)
	}
}

// TestTheDecisionIsTheAdministrators: a user who is not an administrator is
// refused before the store hears of it.
func TestTheDecisionIsTheAdministrators(t *testing.T) {
	r := setupDecisionRoutes(t)
	if rec := r.decide("bob", "d-1", `{"decision":"cancel","reason":"nobody is listening"}`); rec.Code != http.StatusForbidden {
		t.Errorf("a user decided: %d %s", rec.Code, rec.Body.String())
	}
	if len(r.fake.requests) != 0 {
		t.Fatal("the store heard a decision from a user")
	}
	if rec := r.decide("denis", "d-1", `{"decision":"cancel","reason":"nobody is listening"}`); rec.Code != http.StatusOK {
		t.Errorf("the administrator could not decide: %d %s", rec.Code, rec.Body.String())
	}
}
