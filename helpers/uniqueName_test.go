package helpers

import "testing"

// A name is numbered past the taken ones, ignoring case, and is taken itself.
func TestUniqueNameNumbersPastTakenOnes(t *testing.T) {
	taken := map[string]bool{"crm": true, "crm 2": true}
	if got := UniqueName("CRM", taken); got != "CRM 3" || !taken["crm 3"] {
		t.Errorf("got %q, taken %v", got, taken)
	}
	if got := UniqueName("CRM", taken); got != "CRM 4" {
		t.Errorf("a second ask: %q", got)
	}
	if got := UniqueName("Budget", taken); got != "Budget" {
		t.Errorf("a free name: %q", got)
	}
}
