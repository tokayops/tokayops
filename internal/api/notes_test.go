package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/tokayops/tokayops/internal/model"
	"github.com/tokayops/tokayops/internal/store"
)

// TestANoteIsSignedByTheCallerAndBounded. The thread under a card renders
// notes, so a note is written through the domain's door: signed by the person
// who called - the body's actor is not read - and no longer than the thread
// can carry.
func TestANoteIsSignedByTheCallerAndBounded(t *testing.T) {
	api, s, e := setupTestAPI(t)
	s.CreateUser(&model.User{ID: "u-nina", Name: "Nina", Role: model.UserRoleAdmin})
	if err := s.CreateAlertGroup(&model.AlertGroup{
		ID: "ag-1", AlertKey: "k-1", Status: model.AlertGroupStatusTriggered, Title: "Disk filling up",
	}); err != nil {
		t.Fatalf("create the group: %v", err)
	}

	post := func(group, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/alert-groups/"+group+"/notes",
			bytes.NewReader([]byte(body)))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetParamNames("id")
		c.SetParamValues(group)
		c.Set("user_id", "u-nina")
		if err := api.AddAlertGroupNote(c); err != nil {
			t.Fatalf("handler: %v", err)
		}
		return rec
	}

	rec := post("ag-1", `{"message":"looking into it","actor":"<!channel>"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a note answered %d: %s", rec.Code, rec.Body)
	}
	var event model.TimelineEvent
	if err := json.Unmarshal(rec.Body.Bytes(), &event); err != nil {
		t.Fatalf("read the note back: %v", err)
	}
	if event.Actor != "Nina" || event.Message != "looking into it" || event.Type != model.TimelineEventNote {
		t.Fatalf("the note was recorded as %+v", event)
	}

	for name, tc := range map[string]struct {
		group, body string
		want        int
	}{
		"an empty note":      {"ag-1", `{"message":""}`, http.StatusBadRequest},
		"a note too long":    {"ag-1", `{"message":"` + strings.Repeat("x", store.NoteLimit+1) + `"}`, http.StatusBadRequest},
		"the longest note":   {"ag-1", `{"message":"` + strings.Repeat("x", store.NoteLimit) + `"}`, http.StatusCreated},
		"a group nobody has": {"ag-9", `{"message":"hello"}`, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := post(tc.group, tc.body); rec.Code != tc.want {
				t.Fatalf("%s answered %d, want %d: %s", name, rec.Code, tc.want, rec.Body)
			}
		})
	}
}
