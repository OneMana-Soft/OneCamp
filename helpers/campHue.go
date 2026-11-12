package helpers

import "slices"

// CampHues are the camp palette's six identity hues, in the chart order (the
// web's lib/campHue.ts, whose order its hash indexes): what a template, an
// event or a calendar may be coloured by name.
var CampHues = []string{"sky", "moss", "sun", "dusk", "berry", "lake"}

// IsCampHue is whether h names one of them.
func IsCampHue(h string) bool { return slices.Contains(CampHues, h) }

// IsHexColour is whether c is a colour of its own as the server keeps one:
// "#rrggbb", in lower case, so one colour is written one way (an event's own
// colour and a person's recent colours, business/Calendar).
func IsHexColour(c string) bool {
	if len(c) != 7 || c[0] != '#' {
		return false
	}
	for i := 1; i < len(c); i++ {
		if !('0' <= c[i] && c[i] <= '9' || 'a' <= c[i] && c[i] <= 'f') {
			return false
		}
	}
	return true
}
