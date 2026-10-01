package helpers

import (
	"regexp"
	"strconv"
	"strings"
)

// ReleaseVersion is the release this binary was built from, stamped by the
// build server with -ldflags "-X .../helpers.ReleaseVersion=v2.33.0". Empty in
// a build from source, which then simply cannot say which release it is.
var ReleaseVersion = ""

var releaseTagRe = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// ReleaseLine is a release tag's edition, "v2" for "v2.33.0", or "" for
// anything that is not a release tag. Pure.
func ReleaseLine(tag string) string {
	m := releaseTagRe.FindStringSubmatch(strings.TrimSpace(tag))
	if m == nil {
		return ""
	}
	return "v" + m[1]
}

// CompareReleaseTags orders two release tags numerically: >0 when a is newer,
// <0 when b is, 0 when equal or when either is not a release tag. Pure.
func CompareReleaseTags(a, b string) int {
	ma := releaseTagRe.FindStringSubmatch(strings.TrimSpace(a))
	mb := releaseTagRe.FindStringSubmatch(strings.TrimSpace(b))
	if ma == nil || mb == nil {
		return 0
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}
