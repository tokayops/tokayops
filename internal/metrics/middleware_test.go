package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestEchoMiddleware(t *testing.T) {
	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/test/path", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	before := getCounterValue(t, HTTPRequestsTotal, "GET", "/test/path", "200")

	req := httptest.NewRequest(http.MethodGet, "/test/path", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	after := getCounterValue(t, HTTPRequestsTotal, "GET", "/test/path", "200")
	if after-before != 1 {
		t.Errorf("http_requests_total should have incremented by 1, got delta=%v", after-before)
	}
}

func TestEchoMiddleware_ErrorStatus(t *testing.T) {
	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/error", func(c echo.Context) error {
		return echo.NewHTTPError(http.StatusNotFound, "not found")
	})

	before := getCounterValue(t, HTTPRequestsTotal, "GET", "/error", "404")

	req := httptest.NewRequest(http.MethodGet, "/error", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	after := getCounterValue(t, HTTPRequestsTotal, "GET", "/error", "404")
	if after-before != 1 {
		t.Errorf("http_requests_total{status=404} should have incremented by 1, got delta=%v", after-before)
	}
}

func TestEchoMiddleware_Duration(t *testing.T) {
	e := echo.New()
	e.Use(EchoMiddleware())
	e.GET("/dur", func(c echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})

	before := getHistogramCount(t, HTTPRequestDuration, "GET", "/dur")

	req := httptest.NewRequest(http.MethodGet, "/dur", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	after := getHistogramCount(t, HTTPRequestDuration, "GET", "/dur")
	if after-before != 1 {
		t.Errorf("http_request_duration_seconds observation count should have incremented by 1, got delta=%v", after-before)
	}
}

func getCounterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := cv.WithLabelValues(labels...).Write(m); err != nil {
		t.Fatalf("failed to read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}

func getHistogramCount(t *testing.T, hv *prometheus.HistogramVec, labels ...string) uint64 {
	t.Helper()
	observer := hv.WithLabelValues(labels...)
	m := &dto.Metric{}
	if h, ok := observer.(interface{ Write(*dto.Metric) error }); ok {
		if err := h.Write(m); err != nil {
			t.Fatalf("failed to read histogram: %v", err)
		}
	} else {
		t.Fatal("histogram does not implement Write")
	}
	return m.GetHistogram().GetSampleCount()
}
