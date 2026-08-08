package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

const resolvePath = "/api/v1/users/resolve"

// The reason this endpoint exists: history names IDs, and an erased user's row
// survives precisely so those IDs stay legible. GET /users/:id is the active
// read and answers 404 for them, which would leave a client unable to tell an
// erased person from a broken reference.
func TestResolveUsersAnswersForErasedUsers(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	s.EraseUser("alex")
	s.AnonymizeUser("alex")

	// The premise: the active read no longer sees them.
	rec := doJSON(t, e, http.MethodGet, "/api/v1/users/alex", nil, "denis")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /users/alex after erasure = %d, want 404", rec.Code)
	}

	rec = doJSON(t, e, http.MethodPost, resolvePath, ResolveUsersRequest{UserIDs: []string{"alex"}}, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("resolve: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out ResolveUsersResponse
	decodeJSON(t, rec, &out)
	if len(out.Users) != 1 || out.Users[0].ID != "alex" {
		t.Fatalf("resolve returned %+v, want the erased user", out.Users)
	}
	if out.Users[0].Name == "" {
		t.Fatal("an erased user must still resolve to whatever anonymization left behind")
	}
}

// A display read must not become a way to read profiles. Asserted on the raw
// body rather than the DTO: adding a field to the struct is exactly the change
// that would slip past a typed assertion.
func TestResolveUsersReturnsOnlyIDAndName(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	rec := doJSON(t, e, http.MethodPost, resolvePath,
		ResolveUsersRequest{UserIDs: []string{"denis"}}, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{"email", "role", "password", "auth_provider", "identities"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("resolve response leaks %q: %s", leaked, body)
		}
	}
}

// Unknown IDs are absent from the answer rather than present as nulls: "not in
// this list" is one state to render, and a placeholder row would have to be
// told apart from a real one.
func TestResolveUsersOmitsUnknownIDs(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	rec := doJSON(t, e, http.MethodPost, resolvePath,
		ResolveUsersRequest{UserIDs: []string{"denis", "ghost"}}, "denis")
	var out ResolveUsersResponse
	decodeJSON(t, rec, &out)
	if len(out.Users) != 1 || out.Users[0].ID != "denis" {
		t.Fatalf("resolve returned %+v, want only the user that exists", out.Users)
	}
}

// An empty request is an empty answer, not null: the client renders a list.
func TestResolveUsersAnswersEmptyArray(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	rec := doJSON(t, e, http.MethodPost, resolvePath, ResolveUsersRequest{}, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"users":[]}` {
		t.Fatalf("body = %s, want an empty array", body)
	}
}

// The endpoint is reachable by any authenticated user, so the array is
// bounded. Duplicates are folded first: a calendar page naturally repeats the
// same person across shifts, and counting the repeats would reject a page that
// asks about a handful of people.
func TestResolveUsersBoundsTheRequestAfterDeduplication(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	repeated := make([]string, maxResolveUserIDs+50)
	for i := range repeated {
		repeated[i] = "denis"
	}
	rec := doJSON(t, e, http.MethodPost, resolvePath, ResolveUsersRequest{UserIDs: repeated}, "denis")
	if rec.Code != http.StatusOK {
		t.Fatalf("a page repeating one person must be accepted, got %d: %s", rec.Code, rec.Body.String())
	}

	distinct := make([]string, maxResolveUserIDs+1)
	for i := range distinct {
		distinct[i] = "user-" + strconv.Itoa(i)
	}
	rec = doJSON(t, e, http.MethodPost, resolvePath, ResolveUsersRequest{UserIDs: distinct}, "denis")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 past the limit, got %d: %s", rec.Code, rec.Body.String())
	}
	var body ErrorResponse
	decodeJSON(t, rec, &body)
	if body.Code != CodeValidationFailed {
		t.Fatalf("code = %q, want %q", body.Code, CodeValidationFailed)
	}
}

// Resolving names is what every viewer of a calendar needs, so it carries the
// same permission as viewing a user and returns strictly less.
func TestResolveUsersIsReadableByAnyMember(t *testing.T) {
	_, s, e, _ := setupScheduleAPI(t)
	defer s.Close()

	rec := doJSON(t, e, http.MethodPost, resolvePath,
		ResolveUsersRequest{UserIDs: []string{"denis"}}, "alex")
	if rec.Code != http.StatusOK {
		t.Fatalf("member resolve: want 200, got %d: %s", rec.Code, rec.Body.String())
	}
}
