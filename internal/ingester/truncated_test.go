package ingester

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/store"
)

// truncatedNotifications reads the counter the way Prometheus does - from what
// the registry gathers - so a counter that is incremented and not registered,
// or registered under another name, reads as nothing.
func truncatedNotifications(t *testing.T) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "alertmanager_truncated_notifications_total" {
			continue
		}
		var total float64
		for _, m := range family.GetMetric() {
			total += m.GetCounter().GetValue()
		}
		return total
	}
	t.Fatal("alertmanager_truncated_notifications_total is not exported")
	return 0
}

// TestANotificationCutShortIsCounted. max_alerts is outside the contract, and
// the counter is the only place an operator can see it was set: the payload
// is otherwise applied as it came.
func TestANotificationCutShortIsCounted(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	ing := NewIngester(s, &config.Config{}, &mockSecretValidator{secrets: map[string]bool{"secret123": true}})
	e := echo.New()
	ing.RegisterRoutes(e)

	send := func(payload string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("the webhook answered %d: %s", rec.Code, rec.Body.String())
		}
	}

	before := truncatedNotifications(t)
	send(`{"status":"firing","groupKey":"whole","truncatedAlerts":0,
		"alerts":[{"status":"firing","labels":{"alertname":"A","team":"devops"},"fingerprint":"fp-whole"}]}`)
	if got := truncatedNotifications(t) - before; got != 0 {
		t.Fatalf("a whole notification was counted as cut short (%v)", got)
	}

	send(`{"status":"firing","groupKey":"cut","truncatedAlerts":2,
		"alerts":[{"status":"firing","labels":{"alertname":"A","team":"devops"},"fingerprint":"fp-cut"}]}`)
	if got := truncatedNotifications(t) - before; got != 1 {
		t.Fatalf("a notification with two alerts cut off counted %v times, want once", got)
	}

	// Cut short or not, the payload is applied: the group it opened is there.
	if ag, _ := s.GetActiveAlertGroupByAlertKey("cut"); ag == nil {
		t.Fatal("the notification that was cut short opened nothing")
	}
}

// TestAPayloadCannotSayAnAlertWasUnreported. The mark is this system's own
// observation - when Alertmanager stopped reporting an alert - and whoever
// holds the webhook secret must not be able to set it. The ingester reads a
// payload into its own type, so the field has nowhere to land; this holds that
// boundary, on the path that opens an incident and on the one that merges into
// it.
func TestAPayloadCannotSayAnAlertWasUnreported(t *testing.T) {
	s := store.NewMockStore()
	seedDefaultTeams(s)
	ing := NewIngester(s, &config.Config{}, &mockSecretValidator{secrets: map[string]bool{"secret123": true}})
	e := echo.New()
	ing.RegisterRoutes(e)

	send := func(alerts string) {
		t.Helper()
		payload := `{"status":"firing","groupKey":"claimed-unreported",` +
			`"commonLabels":{"team":"devops","severity":"critical","alertname":"A"},` +
			`"alerts":[` + alerts + `]}`
		req := httptest.NewRequest(http.MethodPost, "/webhook/alertmanager?token=secret123", strings.NewReader(payload))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("the webhook answered %d: %s", rec.Code, rec.Body.String())
		}
	}
	claimed := func(fingerprint string) string {
		return `{"status":"firing","labels":{"alertname":"A","team":"devops"},` +
			`"fingerprint":"` + fingerprint + `","unreportedSince":"2020-01-01T00:00:00Z"}`
	}

	send(claimed("fp-1"))
	send(claimed("fp-1") + `,` + claimed("fp-2"))

	ag, err := s.GetActiveAlertGroupByAlertKey("claimed-unreported")
	if err != nil || ag == nil {
		t.Fatalf("the incident was not opened: %v", err)
	}
	if len(ag.Alerts) != 2 {
		t.Fatalf("the incident holds %d alerts, want the two the payloads carried", len(ag.Alerts))
	}
	for _, a := range ag.Alerts {
		if a.UnreportedSince != nil {
			t.Errorf("%s was recorded as unreported since %v, on a payload's say-so",
				a.Fingerprint, a.UnreportedSince)
		}
	}
}
