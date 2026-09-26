package integrations

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tokayops/tokayops/internal/model"
)

// testAccountSID has the shape of a Twilio account id. It is put together
// here rather than written out, because a literal of that shape is what secret
// scanners look for, and a fixture that trips them blocks every push.
var testAccountSID = "AC" + strings.Repeat("0", 31) + "1"

func validTwilio() model.TwilioConfig {
	return model.TwilioConfig{
		AccountSID: testAccountSID, AuthToken: "token",
		FromNumber: "+15005550006", Coverage: []string{"+1", "+7"}, CPS: 1, MaxConcurrent: 2,
	}
}

func TestTwilioConfigValidation(t *testing.T) {
	d, ok := Get(model.IntegrationTypeTwilio)
	if !ok {
		t.Fatal("twilio is not registered")
	}
	if d.Direction != model.IntegrationDirectionOutbound {
		t.Fatalf("direction = %s", d.Direction)
	}
	check := func(c model.TwilioConfig, isUpdate bool) error {
		raw, _ := json.Marshal(c)
		return d.ValidateConfig(raw, isUpdate)
	}
	if err := check(validTwilio(), false); err != nil {
		t.Fatalf("a valid config was refused: %v", err)
	}

	for name, mutate := range map[string]func(*model.TwilioConfig){
		"account sid":        func(c *model.TwilioConfig) { c.AccountSID = "AC123" },
		"missing token":      func(c *model.TwilioConfig) { c.AuthToken = "" },
		"masked token":       func(c *model.TwilioConfig) { c.AuthToken = model.MaskedSecret },
		"local from number":  func(c *model.TwilioConfig) { c.FromNumber = "84155550100" },
		"no coverage":        func(c *model.TwilioConfig) { c.Coverage = nil },
		"coverage not +":     func(c *model.TwilioConfig) { c.Coverage = []string{"7"} },
		"cps zero":           func(c *model.TwilioConfig) { c.CPS = 0 },
		"cps above the self": func(c *model.TwilioConfig) { c.CPS = 6 },
		"no concurrency":     func(c *model.TwilioConfig) { c.MaxConcurrent = 0 },
		"negative priority":  func(c *model.TwilioConfig) { c.Priority = -1 },
	} {
		c := validTwilio()
		mutate(&c)
		if err := check(c, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	// On update the token may stay as it is.
	c := validTwilio()
	c.AuthToken = model.MaskedSecret
	if err := check(c, true); err != nil {
		t.Fatalf("update keeping the token: %v", err)
	}
	// "+" alone covers every number.
	c = validTwilio()
	c.Coverage = []string{"+"}
	if err := check(c, false); err != nil {
		t.Fatalf("world coverage: %v", err)
	}
}
