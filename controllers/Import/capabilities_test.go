package controllers

import (
	"testing"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
)

// The providers list names what each provider brings, and the web app offers
// a provider's pages from the name "pages" (docs programme C4): a capability
// without a name is one the admin is never offered.
func TestEveryImportCapabilityHasAName(t *testing.T) {
	for bit := importProvider.CapTeams; bit <= importProvider.CapPages; bit <<= 1 {
		if capabilityNameMap[bit] == "" {
			t.Errorf("capability %b has no name", bit)
		}
	}
	if got := capabilityNames(importProvider.CapPages); len(got) != 1 || got[0] != "pages" {
		t.Fatalf("CapPages is named %v, want [pages]", got)
	}
}
