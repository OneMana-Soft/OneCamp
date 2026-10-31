package linear

import "testing"

// A deactivated Linear user is someone who left, never invited; a guest
// is outside the company.
func TestADeactivatedLinearUserLeft(t *testing.T) {
	if su := (linearUser{ID: "u1", Name: "Ada", Active: false}).source(); !su.Left {
		t.Errorf("deactivated: %+v, want Left", su)
	}
	if su := (linearUser{ID: "u2", Name: "Bo", Active: true, Guest: true}).source(); su.Left || !su.IsExternal {
		t.Errorf("active guest: %+v, want not Left and external", su)
	}
}
