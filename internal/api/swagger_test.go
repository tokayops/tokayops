package api

import (
	"encoding/json"
	"os"
	"testing"
)

// The generated description is a build artifact that is easy to leave behind:
// a route added without its annotation, or an annotation added without
// regenerating, both leave docs/swagger.json describing yesterday's API. The
// delivery routes are the ones this checks, because three of them went a
// whole sprint without any description at all.
func TestTheSwaggerDescribesEveryDeliveryRoute(t *testing.T) {
	raw, err := os.ReadFile("../../docs/swagger.json")
	if err != nil {
		t.Fatalf("read the generated swagger: %v", err)
	}
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the generated swagger: %v", err)
	}
	for path, method := range map[string]string{
		"/api/v1/deliveries":                                       "get",
		"/api/v1/deliveries/{id}":                                  "get",
		"/api/v1/alert-groups/{id}/deliveries":                     "get",
		"/api/v1/integrations/{id}/deliveries":                     "get",
		"/api/v1/integrations/{id}/deliveries/{deliveryId}":        "get",
		"/api/v1/integrations/{id}/deliveries/{deliveryId}/replay": "post",
	} {
		ops, ok := doc.Paths[path]
		if !ok {
			t.Errorf("%s is not described", path)
			continue
		}
		if _, ok := ops[method]; !ok {
			t.Errorf("%s %s is not described", method, path)
		}
	}
}
