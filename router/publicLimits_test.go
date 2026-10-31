package router

import (
	"regexp"
	"strings"
	"testing"
)

// rootRoute matches a route registered on the root router in router.go's
// source, with any middleware it is registered With.
var rootRoute = regexp.MustCompile(`^\s*router\.(?:With\([^\n]*?\)\.)*(?:Get|Post|Put|Patch|Delete|Head|Options)\(\s*"([^"]*)"`)

// Routes whose handlers count every ask per address themselves, by part
// (redisStore.AllowPerAddress), so a middleware would count them twice: each
// by its path, with the handler that counts it. A new route on these
// addresses fails below until it is listed here or carries a middleware.
var countedInHandler = map[string]string{
	"/public/r/{slug}":                   "guestController.RoomAtAddress",
	"/public/r/{slug}/unlock":            "guestController.UnlockRoom",
	"/public/r/{slug}/join":              "guestController.JoinRoom",
	"/public/r/{slug}/knock/{ticket}":    "guestController.RoomKnockAnswer",
	"/public/p/{slug}":                   "publishController.PublishedPage",
	"/public/p/{slug}/unlock":            "publishController.PublishedPageUnlock",
	"/public/p/{slug}/state":             "publishController.PublishedPageState",
	"/public/p/{slug}/files/{obj_uuid}":  "publishController.PublishedFile",
	"/public/d/{token}":                  "publishController.LinkPage",
	"/public/d/{token}/unlock":           "publishController.LinkPageUnlock",
	"/public/d/{token}/files/{obj_uuid}": "publishController.LinkFile",
}

// Unlimited on purpose, with the reason.
var unlimited = map[string]string{
	"/public/email/logo": "email clients' image proxies fetch it for many people from a few addresses; it is one static file",
}

// Audit A3 (22 Oct 2026): nine guest GETs relied on limits that never counted
// a read, and three guest and public routes had none. Every root route under
// /guest/ or /public/ now carries a per-address limit (a RateLimit or a
// ReadLimit middleware), or is one its handler counts, or is unlimited with a
// reason.
func TestEveryGuestAndPublicRouteIsLimitedPerAddress(t *testing.T) {
	checked := 0
	seen := map[string]bool{}
	for _, line := range routerSource(t) {
		m := rootRoute.FindStringSubmatch(line)
		if m == nil || !(strings.HasPrefix(m[1], "/guest/") || strings.HasPrefix(m[1], "/public/")) {
			continue
		}
		checked++
		seen[m[1]] = true
		if strings.Contains(line, "RateLimit(") || strings.Contains(line, "ReadLimit(") {
			continue
		}
		if _, ok := unlimited[m[1]]; ok {
			continue
		}
		if handler, ok := countedInHandler[m[1]]; ok {
			if !strings.Contains(line, ", "+handler+")") {
				t.Errorf("%s is listed as counted by %s, but registers another handler: %s", m[1], handler, strings.TrimSpace(line))
			}
			continue
		}
		t.Errorf("%s has no per-address limit: add LinkReadLimit or FileReadLimit (middleware/publicReadLimit.go), or IPRateLimit for a write: %s", m[1], strings.TrimSpace(line))
	}
	if checked == 0 {
		t.Fatal("matched no guest or public routes; the pattern no longer fits the router")
	}
	for _, list := range []map[string]string{countedInHandler, unlimited} {
		for path := range list {
			if !seen[path] {
				t.Errorf("%s is listed here but no route registers it: take it off the list", path)
			}
		}
	}
}
