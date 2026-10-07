package main

import "testing"

// The phone is a step of its own kind and never a direct message. The handoff
// fan-out goes through every provider that carries "dm", and a shift change
// announced by phone would wake whoever is coming on duty.
func TestTheCallProviderCarriesOnlyCalls(t *testing.T) {
	phone, ok := channelCatalog().Capabilities("phone")
	if !ok {
		t.Fatal("the phone is not in the catalogue")
	}
	if !phone.Carries("call") {
		t.Fatal("the phone does not carry calls")
	}
	if phone.Carries("dm") || phone.Carries("channel") {
		t.Fatalf("the phone carries %v", phone.SupportedTargetKinds)
	}
}
