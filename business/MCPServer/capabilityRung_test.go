package business

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// AuthorizeToolCall's capability rung asks authz.Can about the principal's
// whole account (aiBusiness.BuildUserInfoByUserUUID: their admin flag and roles
// included). A token acts at most as a member (helpers.ActsThroughToken), so
// while the rung reads the account that way, no MCP tool may need a capability:
// an admin's would pass for their token. The day a tool needs one, the rung has
// to ask about the account as a member first, and this test says so.
func TestNoMCPToolNeedsACapabilityWhileTheRungReadsTheWholeAccount(t *testing.T) {
	raw, err := os.ReadFile("authorize.go")
	if err != nil {
		t.Fatal(err)
	}
	src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")
	start := strings.Index(src, "spec.RequiredCapability")
	if start < 0 {
		t.Fatal("authorize.go has no capability rung; this test has gone stale")
	}
	rung := src[start:]
	if end := strings.Index(rung, "PrincipalCanReach("); end > 0 {
		rung = rung[:end]
	}
	wholeAccount := strings.Contains(rung, "BuildUserInfoByUserUUID(") && !strings.Contains(rung, "ActsThroughToken(")
	if !wholeAccount {
		t.Skip("the capability rung no longer reads the principal's whole account")
	}
	for _, b := range bridgedTools {
		spec, err := specFor(b)
		if err != nil {
			t.Fatalf("%s: %v", b.Tool, err)
		}
		if strings.TrimSpace(spec.RequiredCapability) != "" {
			t.Errorf("%s needs the capability %q, which the rung checks against its owner's whole account, "+
				"so an admin's token holds it: ask about the account as a member first (helpers.ActsThroughToken)",
				b.Tool, spec.RequiredCapability)
		}
	}
	if len(bridgedTools) == 0 {
		t.Fatal("no bridged tools; this test has gone stale")
	}
}
