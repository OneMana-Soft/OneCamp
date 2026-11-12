package helpers

import (
	"strings"
	"time"
)

// pickedDay is how a day a person picks travels: "2006-01-02".
const pickedDay = "2006-01-02"

// TargetDay is a target day as a person sets one (a milestone's, a
// release's): nil or blank is none (nil, true); a real day from 1970 on is
// that day, written as "2006-01-02"; anything else can't be read (nil, false).
// Pure.
func TargetDay(raw *string) (*string, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil, true
	}
	d, err := time.Parse(pickedDay, strings.TrimSpace(*raw))
	if err != nil || d.Year() < 1970 {
		return nil, false
	}
	day := d.Format(pickedDay)
	return &day, true
}
