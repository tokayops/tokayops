package api

import "testing"

type callCaps struct{}

func (callCaps) Capabilities(name string) (ProviderCapability, bool) {
	switch name {
	case "phone":
		return ProviderCapability{Name: "phone", SupportedTargetKinds: []string{"call"}}, true
	case "slack":
		return ProviderCapability{Name: "slack", SupportedTargetKinds: []string{"channel", "dm"}}, true
	}
	return ProviderCapability{}, false
}

func (callCaps) AllCapabilities() []ProviderCapability { return nil }

// A call goes to a person or to whoever a schedule has on call, says the same
// few words for every alert, and is a step only the phone carries.
func TestACallStepIsValidatedAsACall(t *testing.T) {
	for _, tc := range []struct {
		name string
		step PolicyStepRequest
		ok   bool
	}{
		{"to a person", PolicyStepRequest{Provider: "phone", TargetKind: "call", TargetType: "user", TargetID: "u1"}, true},
		{"to the schedule", PolicyStepRequest{Provider: "phone", TargetKind: "call", TargetType: "schedule"}, true},
		{"to a channel", PolicyStepRequest{Provider: "phone", TargetKind: "call", TargetType: "channel", TargetID: "C1"}, false},
		{"with a message", PolicyStepRequest{Provider: "phone", TargetKind: "call", TargetType: "user", TargetID: "u1",
			Message: "hello"}, false},
		{"through slack", PolicyStepRequest{Provider: "slack", TargetKind: "call", TargetType: "user", TargetID: "u1"}, false},
		{"a phone dm", PolicyStepRequest{Provider: "phone", TargetKind: "dm", TargetType: "user", TargetID: "u1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicyStep(tc.step, callCaps{})
			if (err == nil) != tc.ok {
				t.Fatalf("validate = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}
