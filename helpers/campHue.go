package helpers

import "slices"

// CampHues are the camp palette's six identity hues, in the chart order (the
// web's lib/campHue.ts, whose order its hash indexes): what a template, an
// event or a calendar may be coloured by name.
var CampHues = []string{"sky", "moss", "sun", "dusk", "berry", "lake"}

// IsCampHue is whether h names one of them.
func IsCampHue(h string) bool { return slices.Contains(CampHues, h) }
