package helpers

import "testing"

// A colour of its own is "#rrggbb" in lower case, and nothing else: one
// colour is written one way.
func TestIsHexColour(t *testing.T) {
	for c, want := range map[string]bool{
		"#1a73e8": true, "#000000": true, "#ffffff": true,
		"#1A73E8": false, "#1a73e": false, "#1a73e80": false, "1a73e8": false, "#gggggg": false,
		"": false, "#": false, "sky": false, " #1a73e8": false, "#1a73e8 ": false, "#fff": false,
	} {
		if got := IsHexColour(c); got != want {
			t.Errorf("%q: %v, want %v", c, got, want)
		}
	}
}
