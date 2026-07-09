package dispatcher

import (
	"testing"

	"github.com/tokayops/tokayops/internal/config"
	"github.com/tokayops/tokayops/internal/store"
)

func mustNewDispatcher(t *testing.T, s store.StoreInterface, cfg *config.Config) *Dispatcher {
	t.Helper()
	d, err := NewDispatcher(s, cfg)
	if err != nil {
		t.Fatalf("NewDispatcher failed: %v", err)
	}
	return d
}
