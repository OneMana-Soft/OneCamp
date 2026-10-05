package registry

import (
	"strings"
	"testing"
)

// Catches typos in Spec declarations: namespaces must be unique so a
// SCAN of the namespace doesn't collide with another feature's keys.
func TestNamespacesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range all() {
		if seen[s.Namespace] {
			t.Fatalf("duplicate namespace %q", s.Namespace)
		}
		seen[s.Namespace] = true
		if !strings.Contains(s.Namespace, ":") && s.Namespace != "" {
			// Single-word namespace is OK but discouraged; warn loudly.
			t.Logf("warning: namespace %q has no colon prefix", s.Namespace)
		}
		if s.Description == "" {
			t.Fatalf("spec %q has no Description; document why this key exists", s.Namespace)
		}
		if s.Category == "" {
			t.Fatalf("spec %q has no Category", s.Namespace)
		}
	}
}

// Build joins segments with the colon separator and rejects mismatched arity.
func TestBuildArity(t *testing.T) {
	got := UserRefreshToken.Build("u", "d")
	want := "auth:refresh:u:d"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}

	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on arity mismatch")
		}
	}()
	_ = UserRefreshToken.Build("only-one")
}

// Pattern returns the SCAN-friendly wildcard.
func TestPattern(t *testing.T) {
	got := ProjectTasks.Pattern("p")
	want := "project:tasks:p:*"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := ProjectTasks.PatternAll(); got != "project:tasks:*" {
		t.Fatalf("PatternAll mismatch: %q", got)
	}
}

// All TTLs should be set explicitly. A 0 TTL is OK only when paired
// with a Description that explains the manual-cleanup contract.
func TestTTLPositive(t *testing.T) {
	for _, s := range all() {
		if s.TTL <= 0 {
			t.Errorf("spec %q has non-positive TTL; document manual cleanup or set a TTL", s.Namespace)
		}
	}
}

// All returns the master list. Callers should treat the slice as
// read-only.
func all() []Spec {
	return []Spec{
		UserRefreshToken,
		LoginRate,
		SSOState,
		UserProfile,
		UserDgraphProfile,
		UserSidebar,
		UserChannels,
		UserProjects,
		ChannelBasicInfo,
		ProjectTasks,
		AISession,
		AIRate,
		AIModelList,
		AIOllamaLatest,
		AIWebSearch,
		AIUnifiedSearch,
		AIAdminRate,
		AIRecapLock,
		AIStreamStop,
		AIStreamLive,
		AIMemoryWatermark,
		AIMemoryExtractLock,
		AIMemoryBackfillLock,
		AIMemoryBackfillStatus,
		AISelfTestLock,
		AISelfTestStatus,
		AIMemoryDigestLock,
		AITeamReportLock,
		AINudgeRunLock,
		TranscriptionAdminRate,
		WebhookRateLimit,
		ApiTokenRate,
		MCPToolReadRate,
		MCPToolWriteRate,
		ApiTokenTouch,
		AITokenBudget,
		AIUserTokenBudget,
		AITokenUserLeaderboard,
		AIAgentTokenBudget,
		AIChannelTokenBudget,
		AIChannelTokenLeaderboard,
		ImportDiscover,
		ImportRate,
		SlackImportRate,
		ArchiveRate,
		CommandCatalog,
		CommandCatalogVersion,
		CommandRate,
		CommandInteraction,
		CommandOAuthState,
		ConnectorOAuthState,
		PasskeyCeremony,
		ConnectorBriefingDay,
		CollabStateSize,
	}
}
