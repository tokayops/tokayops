package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tokayops/tokayops/internal/model"
)

// The channel's read of a subscriber's configuration: from the database, under
// the caller's context, decrypted, and honest about the difference between "no
// such subscriber" and "the database failed".
func TestASubscriberConfigurationIsReadFromTheDatabase(t *testing.T) {
	s := setupTestDB(t)
	id := subscriber(t, s, "hooks", model.WebhookScopeGlobal, "", true)

	cfg, found, err := s.SubscriberConfig(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if cfg.URL != "https://example.com/hooks" || cfg.Secret != "s" {
		t.Fatalf("configuration read back as %+v: not decrypted, or not this subscriber's", cfg)
	}

	// Rotated through the store, read back fresh: no cache in between.
	raw, _ := json.Marshal(model.GenericWebhookConfig{URL: "https://example.com/hooks", Secret: "rotated",
		TimeoutSeconds: 45, CustomHeaders: map[string]string{"X-Team": "sre"}})
	if _, err := s.UpdateIntegration(context.Background(), id, IntegrationPatch{Config: raw}, "test"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	cfg, found, err = s.SubscriberConfig(context.Background(), id)
	if err != nil || !found || cfg.Secret != "rotated" || cfg.TimeoutSeconds != 45 || cfg.CustomHeaders["X-Team"] != "sre" {
		t.Fatalf("after rotation: %+v found=%v err=%v", cfg, found, err)
	}

	// No such subscriber: found=false and no error.
	if _, found, err := s.SubscriberConfig(context.Background(), "nobody"); err != nil || found {
		t.Fatalf("a subscriber that does not exist: found=%v err=%v", found, err)
	}

	// An integration of another type is not a subscriber either.
	slackCfg, _ := json.Marshal(model.SlackConfig{Token: "xoxb-1"})
	slack := &model.Integration{Type: model.IntegrationTypeSlack, Name: "slack", Enabled: true, Config: slackCfg}
	if err := s.CreateIntegration(slack); err != nil {
		t.Fatalf("create slack: %v", err)
	}
	if _, found, err := s.SubscriberConfig(context.Background(), slack.ID); err != nil || found {
		t.Fatalf("a Slack integration read as a subscriber: found=%v err=%v", found, err)
	}

	// The read is bounded by the caller's context: a context already over
	// answers an error, not a subscriber and not "none".
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	if _, found, err := s.SubscriberConfig(ctx, id); err == nil || found {
		t.Fatalf("a read under a dead context answered found=%v err=%v", found, err)
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("dead context answered %v (a driver error is acceptable)", err)
	}
}
