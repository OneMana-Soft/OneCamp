package helpers

import "testing"

func TestIsValidName(t *testing.T) {
	for _, ok := range []string{"qa", "launch-week", "Q4 launch", "design_ops", "विपणन", " ops "} {
		if !IsValidName(ok) {
			t.Errorf("%q was refused", ok)
		}
	}
	for _, bad := range []string{"", "a", "  a ", "launch/week", "<b>", "x234567890123456789012345678901234567890y"} {
		if IsValidName(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}
