package router

import (
	"regexp"
	"strings"
	"testing"
)

// Every write a guest link allows has its own limit, so approving a sprint's
// tasks never uses up the comments, and none is held to the sign-in limit.
func TestEachGuestWriteHasItsOwnLimit(t *testing.T) {
	writes := map[string]string{
		`"/guest/meet/{token}/join"`:                      "GuestJoin",
		`"/guest/collab/{token}"`:                         "GuestOpenLive",
		`"/guest/channel/{token}"`:                        "GuestMessage",
		`"/guest/project/{token}/task/{task_id}/comment"`: "GuestComment",
		`"/guest/project/{token}/task/{task_id}/review"`:  "GuestApproval",
		`"/guest/doc-comments/{token}"`:                   "GuestDocComment",
	}
	post := regexp.MustCompile(`\.Post\(("[^"]+")`)
	seen := map[string]bool{}
	for i, line := range routerSource(t) {
		m := post.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		kind, ok := writes[m[1]]
		if !ok {
			continue
		}
		seen[m[1]] = true
		if !strings.Contains(line, "GuestRateLimit(customMiddleware."+kind+")") {
			t.Errorf("router.go:%d %s is not limited as a %s: %s", i+1, m[1], kind, strings.TrimSpace(line))
		}
	}
	for path := range writes {
		if !seen[path] {
			t.Errorf("%s is not registered as a POST", path)
		}
	}
}
