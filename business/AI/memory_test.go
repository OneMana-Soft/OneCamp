package business

import (
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	prefModels "github.com/akashc777/OneCamp/models/postgres/UserNotificationPreference"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// helper: build a UserInfo with given pg id, accessible channels, projects, DMs.
func makeUser(pgID uuid.UUID, channels, projects []string, grpIDs []string) *userModels.UserInfo {
	var chs []*dgraphStruct.DgraphChannel
	for _, c := range channels {
		chs = append(chs, &dgraphStruct.DgraphChannel{Uuid: c})
	}
	var prs []*dgraphStruct.DgraphProject
	for _, p := range projects {
		prs = append(prs, &dgraphStruct.DgraphProject{Uuid: p})
	}
	var dms []*dgraphStruct.DgraphDm
	for _, g := range grpIDs {
		dms = append(dms, &dgraphStruct.DgraphDm{GroupingId: g})
	}
	return &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: pgID},
		UserDgraphInfo: dgraphStruct.DgraphUser{
			Uuid:     pgID.String(),
			Channels: chs,
			Projects: prs,
			DMs:      dms,
		},
	}
}

func ptrUUID(u uuid.UUID) *uuid.UUID { return &u }

func TestMemoryItemVisibleTo_ChannelScope(t *testing.T) {
	chID := uuid.New()
	user := makeUser(uuid.New(), []string{chID.String()}, nil, nil)

	// Item scoped to a channel the user belongs to → visible.
	visible := &memoryModels.MemoryItem{ChannelUUID: ptrUUID(chID)}
	if !memoryItemVisibleTo(visible, user) {
		t.Error("expected channel-scoped item in accessible channel to be visible")
	}

	// Item scoped to a different channel → NOT visible.
	hidden := &memoryModels.MemoryItem{ChannelUUID: ptrUUID(uuid.New())}
	if memoryItemVisibleTo(hidden, user) {
		t.Error("expected channel-scoped item in inaccessible channel to be hidden")
	}
}

func TestMemoryItemVisibleTo_ProjectScope(t *testing.T) {
	prID := uuid.New()
	user := makeUser(uuid.New(), nil, []string{prID.String()}, nil)

	if !memoryItemVisibleTo(&memoryModels.MemoryItem{ProjectUUID: ptrUUID(prID)}, user) {
		t.Error("expected project-scoped item in accessible project to be visible")
	}
	if memoryItemVisibleTo(&memoryModels.MemoryItem{ProjectUUID: ptrUUID(uuid.New())}, user) {
		t.Error("expected project-scoped item in inaccessible project to be hidden")
	}
}

func TestMemoryItemVisibleTo_DMScope(t *testing.T) {
	grp := "aaa bbb"
	user := makeUser(uuid.New(), nil, nil, []string{grp})

	if !memoryItemVisibleTo(&memoryModels.MemoryItem{ChatGrpID: grp}, user) {
		t.Error("expected DM-scoped item in accessible grouping to be visible")
	}
	if memoryItemVisibleTo(&memoryModels.MemoryItem{ChatGrpID: "ccc ddd"}, user) {
		t.Error("expected DM-scoped item in inaccessible grouping to be hidden")
	}
}

func TestMemoryItemVisibleTo_OwnershipDoesNotBypassScope(t *testing.T) {
	pgID := uuid.New()
	// User is in NO scopes at all.
	user := makeUser(pgID, nil, nil, nil)

	// PRIVACY INVARIANT: owning or authoring an item must NOT grant access
	// once the user has left (here: was never in) the item's scope. Otherwise
	// a stale claim leaks the item's content cross-scope.
	chID := uuid.New()
	ownedButGone := &memoryModels.MemoryItem{ChannelUUID: ptrUUID(chID), OwnerID: ptrUUID(pgID)}
	if memoryItemVisibleTo(ownedButGone, user) {
		t.Error("owning an item in a channel the user left must NOT grant access")
	}
	createdButGone := &memoryModels.MemoryItem{ChannelUUID: ptrUUID(chID), CreatedBy: ptrUUID(pgID)}
	if memoryItemVisibleTo(createdButGone, user) {
		t.Error("authoring an item in a channel the user left must NOT grant access")
	}

	// But when the user IS still in the scope, they see it (owner or not).
	inScope := makeUser(pgID, []string{chID.String()}, nil, nil)
	if !memoryItemVisibleTo(ownedButGone, inScope) {
		t.Error("owner who is still a channel member must see their item")
	}

	// Unscoped + not in any scope → not visible (fail-closed).
	if memoryItemVisibleTo(&memoryModels.MemoryItem{OwnerID: ptrUUID(pgID)}, user) {
		t.Error("unscoped item must be hidden (fail-closed)")
	}
}

func TestIsMemoryIntent(t *testing.T) {
	yes := []string{
		"what did we decide about the API?",
		"what's still open?",
		"who is responsible for the migration",
		"any blockers on the release",
		"what are my action items",
	}
	for _, q := range yes {
		if !isMemoryIntent(q) {
			t.Errorf("expected memory intent for %q", q)
		}
	}
	no := []string{
		"hi there",
		"how do I write a goroutine",
		"explain the architecture",
		"what's the weather",
	}
	for _, q := range no {
		if isMemoryIntent(q) {
			t.Errorf("did NOT expect memory intent for %q", q)
		}
	}
}

func TestMemoryDedupHash_StableAndScoped(t *testing.T) {
	s1 := MemoryScope{ChannelUUID: "ch1"}
	h1 := memoryDedupHash("decision", "Ship the  API   redesign", s1)
	// Whitespace-normalized + case-insensitive → same hash.
	h2 := memoryDedupHash("decision", "ship the api redesign", s1)
	if h1 != h2 {
		t.Error("dedup hash should be normalization-invariant")
	}
	// Different scope → different hash.
	h3 := memoryDedupHash("decision", "ship the api redesign", MemoryScope{ChannelUUID: "ch2"})
	if h1 == h3 {
		t.Error("dedup hash must incorporate scope")
	}
	// Different kind → different hash.
	h4 := memoryDedupHash("commitment", "ship the api redesign", s1)
	if h2 == h4 {
		t.Error("dedup hash must incorporate kind")
	}
}

func TestParseExtractedItems(t *testing.T) {
	raw := "```json\n[" +
		`{"kind":"decision","content":"Use Postgres","confidence":90},` +
		`{"kind":"commitment","content":"Alice ships Friday","owner":"Alice","due":"2026-06-05","confidence":80},` +
		`{"kind":"bogus","content":"ignored"},` +
		`{"kind":"question","content":""}` +
		"]\n```"
	items := parseExtractedItems(raw)
	if len(items) != 2 {
		t.Fatalf("expected 2 valid items (bogus kind + empty content dropped), got %d", len(items))
	}
	if items[0].Kind != "decision" || items[1].Kind != "commitment" {
		t.Errorf("unexpected parsed kinds: %+v", items)
	}
}

func TestParseExtractedItems_EmptyArray(t *testing.T) {
	if items := parseExtractedItems("[]"); len(items) != 0 {
		t.Errorf("expected 0 items for empty array, got %d", len(items))
	}
	if items := parseExtractedItems("not json at all"); items != nil {
		t.Errorf("expected nil for non-JSON, got %v", items)
	}
}

// --- batched memory worker helpers ---

func TestFormatBatchWindow(t *testing.T) {
	items := []ai.ScopedContent{
		{AuthorName: "Alice", ContentText: "We should ship Friday"},
		{AuthorName: "", ContentText: "agreed"}, // empty author → "participant"
		{AuthorName: "Bob", ContentText: "   "}, // blank content → skipped
		{AuthorName: "Carol", ContentText: "what about QA?"},
	}
	out := formatBatchWindow(items)

	if !strings.Contains(out, "Alice: We should ship Friday") {
		t.Errorf("expected Alice line, got:\n%s", out)
	}
	if !strings.Contains(out, "participant: agreed") {
		t.Errorf("expected empty-author line to render as participant, got:\n%s", out)
	}
	if strings.Contains(out, "Bob:") {
		t.Errorf("blank-content line should be skipped, got:\n%s", out)
	}
	if !strings.Contains(out, "Carol: what about QA?") {
		t.Errorf("expected Carol line, got:\n%s", out)
	}
}

func TestFormatBatchWindow_RespectsCharCap(t *testing.T) {
	// Build content that far exceeds the prompt cap; the formatter must
	// stop before maxBatchPromptChars rather than emit everything.
	big := strings.Repeat("x", 500)
	var items []ai.ScopedContent
	for i := 0; i < 200; i++ { // ~100k chars of raw content
		items = append(items, ai.ScopedContent{AuthorName: "U", ContentText: big})
	}
	out := formatBatchWindow(items)
	if len(out) > maxBatchPromptChars {
		t.Errorf("formatted window %d chars exceeds cap %d", len(out), maxBatchPromptChars)
	}
	if len(out) == 0 {
		t.Error("expected non-empty window")
	}
}

func TestMemorySourceTypeForScope(t *testing.T) {
	if got := memorySourceTypeForScope("chat_grp_id"); got != memoryModels.SourceChat {
		t.Errorf("group scope should map to SourceChat, got %q", got)
	}
	if got := memorySourceTypeForScope("channel_uuid"); got != memoryModels.SourcePost {
		t.Errorf("channel scope should map to SourcePost, got %q", got)
	}
	if got := memorySourceTypeForScope("project_uuid"); got != memoryModels.SourcePost {
		t.Errorf("project scope should map to SourcePost, got %q", got)
	}
}

// --- chunked extraction engine ---

func TestFormatChunk_SplitsAcrossCalls(t *testing.T) {
	// Three lines that individually fit but together exceed a tiny cap.
	items := []ai.ScopedContent{
		{AuthorName: "A", ContentText: strings.Repeat("x", 30)},
		{AuthorName: "B", ContentText: strings.Repeat("y", 30)},
		{AuthorName: "C", ContentText: strings.Repeat("z", 30)},
	}
	// Cap fits ~1 line per chunk (line ≈ "A: " + 30 + "\n" ≈ 34 bytes).
	text1, next1 := formatChunk(items, 0, 40)
	if next1 == 0 || next1 >= len(items) {
		t.Fatalf("expected partial progress, got next=%d", next1)
	}
	if !strings.Contains(text1, "A:") {
		t.Errorf("first chunk should contain first item, got: %q", text1)
	}
	// Continue from the cursor; must eventually consume everything.
	text2, next2 := formatChunk(items, next1, 40)
	if next2 <= next1 {
		t.Fatalf("expected forward progress, next1=%d next2=%d", next1, next2)
	}
	if text2 == "" {
		t.Error("second chunk should be non-empty")
	}
}

func TestFormatChunk_OversizeSingleLineTruncates(t *testing.T) {
	items := []ai.ScopedContent{
		{AuthorName: "A", ContentText: strings.Repeat("x", 500)},
		{AuthorName: "B", ContentText: "short"},
	}
	text, next := formatChunk(items, 0, 50)
	if next != 1 {
		t.Fatalf("expected cursor to advance past the oversize line (next=1), got %d", next)
	}
	if len(text) > 50 {
		t.Errorf("oversize line should be truncated to cap, got %d bytes", len(text))
	}
	if text == "" {
		t.Error("chunk must not be empty (progress guarantee)")
	}
}

func TestRuneSafeTruncate(t *testing.T) {
	// "héllo" — é is 2 bytes. Truncating at 2 must not split it.
	s := "héllo"
	out := runeSafeTruncate(s, 2)
	if !utf8ValidString(out) {
		t.Errorf("runeSafeTruncate produced invalid UTF-8: %q", out)
	}
	if runeSafeTruncate("abc", 10) != "abc" {
		t.Error("no truncation when under cap")
	}
}

func utf8ValidString(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}

// --- ownership intent ---

func TestIsOwnershipIntent(t *testing.T) {
	yes := []string{
		"what are my commitments?",
		"what's on my plate this week",
		"what am i responsible for",
		"show me what i committed to",
	}
	for _, q := range yes {
		if !isOwnershipIntent(q) {
			t.Errorf("expected ownership intent for %q", q)
		}
	}
	no := []string{
		"what did the team decide",
		"how do i deploy",
		"hello there",
	}
	for _, q := range no {
		if isOwnershipIntent(q) {
			t.Errorf("did NOT expect ownership intent for %q", q)
		}
	}
}

// --- digest rendering ---

func TestBuildDigest_OrdersAndCaps(t *testing.T) {
	due := time.Now().Add(-48 * time.Hour)
	items := []*memoryModels.MemoryItem{
		{Kind: memoryModels.KindQuestion, Content: "Is the API frozen?", CreatedAt: time.Now()},
		{Kind: memoryModels.KindCommitment, Content: "Ship the redesign", DueAt: &due, CreatedAt: time.Now()},
	}
	subject, body := buildDigest(items)
	if subject == "" || body == "" {
		t.Fatal("expected non-empty digest")
	}
	// Commitments section must come before questions section.
	ci := strings.Index(body, "commitments")
	qi := strings.Index(body, "Open questions")
	if ci == -1 || qi == -1 || ci > qi {
		t.Errorf("expected commitments before questions, body:\n%s", body)
	}
	if !strings.Contains(body, "Ship the redesign") {
		t.Error("commitment content missing from digest")
	}
}

func TestBuildDigest_Empty(t *testing.T) {
	if s, b := buildDigest(nil); s != "" || b != "" {
		t.Errorf("expected empty digest for no items, got subject=%q body=%q", s, b)
	}
}

func TestDigestWantedToday(t *testing.T) {
	daily := &prefModels.UserNotificationPreference{EmailEnabled: true, EmailDigestFrequency: "daily"}
	weekly := &prefModels.UserNotificationPreference{EmailEnabled: true, EmailDigestFrequency: "weekly"}
	off := &prefModels.UserNotificationPreference{EmailEnabled: true, EmailDigestFrequency: "off"}
	disabled := &prefModels.UserNotificationPreference{EmailEnabled: false, EmailDigestFrequency: "daily"}

	if !digestWantedToday(daily, time.Tuesday) {
		t.Error("daily should fire any day")
	}
	if digestWantedToday(weekly, time.Tuesday) {
		t.Error("weekly should NOT fire on Tuesday")
	}
	if !digestWantedToday(weekly, time.Monday) {
		t.Error("weekly should fire on Monday")
	}
	if digestWantedToday(off, time.Monday) {
		t.Error("off should never fire")
	}
	if digestWantedToday(disabled, time.Monday) {
		t.Error("email-disabled should never fire")
	}
	if digestWantedToday(nil, time.Monday) {
		t.Error("nil prefs (no opt-in) should never fire")
	}
}

// --- scope-anchored GraphRAG helpers ---

func TestResolveScopeFromQuestion(t *testing.T) {
	channels := []scopeRef{
		{UUID: "ch-design", Name: "design"},
		{UUID: "ch-design-system", Name: "design-system"},
		{UUID: "ch-eng", Name: "engineering"},
	}
	projects := []scopeRef{
		{UUID: "pr-apollo", Name: "Apollo"},
	}

	// Longest channel name wins ("design-system" over "design").
	st, scUUID, label := resolveScopeFromQuestion("what's open in the design-system channel?", channels, projects)
	if st != "channel" || scUUID != "ch-design-system" || label != "#design-system" {
		t.Errorf("expected design-system channel, got type=%s uuid=%s label=%s", st, scUUID, label)
	}

	// Channel takes precedence over project when both could match.
	st2, _, _ := resolveScopeFromQuestion("status of engineering", channels, projects)
	if st2 != "channel" {
		t.Errorf("expected channel match, got %s", st2)
	}

	// Project match when no channel referenced.
	st3, uuid3, label3 := resolveScopeFromQuestion("what decisions in Apollo?", channels, projects)
	if st3 != "project" || uuid3 != "pr-apollo" || label3 != "Apollo" {
		t.Errorf("expected Apollo project, got type=%s uuid=%s label=%s", st3, uuid3, label3)
	}

	// No scope referenced → empty.
	st4, _, _ := resolveScopeFromQuestion("how do goroutines work", channels, projects)
	if st4 != "" {
		t.Errorf("expected no scope, got %s", st4)
	}
}

func TestFormatGraphScopeItemsForLLM(t *testing.T) {
	due := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	items := []*dgraphStruct.DgraphMemoryItem{
		{Kind: "decision", Content: "Adopt Postgres for the memory store"},
		{Kind: "commitment", Content: "Wire the backfill endpoint", DueAt: &due,
			Owner: &dgraphStruct.DgraphUser{UserName: "alice"}},
		{Kind: "question", Content: "Do we need pgvector?"},
	}
	out := formatGraphScopeItemsForLLM("#design", items)

	for _, want := range []string{"Open Items in #design", "### Decisions", "### Commitments",
		"### Open Questions", "Adopt Postgres", "@alice", "due 2026-06-05", "pgvector"} {
		if !strings.Contains(out, want) {
			t.Errorf("scope block missing %q in:\n%s", want, out)
		}
	}

	if formatGraphScopeItemsForLLM("#x", nil) != "" {
		t.Error("empty items should produce empty block")
	}
}

func TestTeamReportToHTML(t *testing.T) {
	items := []*dgraphStruct.DgraphMemoryItem{
		{Kind: "decision", Content: "Ship v2"},
		{Kind: "commitment", Content: "Owner does X", Owner: &dgraphStruct.DgraphUser{UserName: "bob"}},
	}
	html := teamReportToHTML("Good progress this week.", items, "design")
	for _, want := range []string{"Weekly Channel Report", "Good progress this week.",
		"✅ Decisions", "Ship v2", "📋 Commitments", "@bob"} {
		if !strings.Contains(html, want) {
			t.Errorf("team report HTML missing %q in:\n%s", want, html)
		}
	}
	// HTML-escaping: a script-y owner/content must not appear raw.
	evil := []*dgraphStruct.DgraphMemoryItem{{Kind: "decision", Content: "<script>x</script>"}}
	if strings.Contains(teamReportToHTML("", evil, "c"), "<script>") {
		t.Error("content must be HTML-stripped in team report")
	}
	// No items → empty.
	if teamReportToHTML("intro", nil, "c") != "" {
		t.Error("no items should yield empty report")
	}
}

func TestTeamReportHour(t *testing.T) {
	t.Setenv("AI_TEAM_REPORT_HOUR", "")
	if teamReportHour() != teamReportDefaultHour {
		t.Errorf("expected default hour %d", teamReportDefaultHour)
	}
	t.Setenv("AI_TEAM_REPORT_HOUR", "14")
	if teamReportHour() != 14 {
		t.Error("expected 14")
	}
	t.Setenv("AI_TEAM_REPORT_HOUR", "99") // out of range → default
	if teamReportHour() != teamReportDefaultHour {
		t.Error("out-of-range should fall back to default")
	}
}

// --- channel-scope permission intersection ---

func TestContainsStr(t *testing.T) {
	list := []string{"a", "b", "c"}
	if !containsStr(list, "b") {
		t.Error("expected 'b' to be found")
	}
	if containsStr(list, "z") {
		t.Error("did not expect 'z'")
	}
	if containsStr(nil, "a") {
		t.Error("nil list should contain nothing")
	}
}

// --- briefing ordering ---

func TestSortBriefingOpenItems(t *testing.T) {
	today := time.Now().Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")

	items := []adapter.MemoryItemView{
		{Kind: "question", Content: "q", CreatedAt: "2026-01-01T00:00:00Z"},
		{Kind: "commitment", Content: "overdue", DueAt: yesterday, CreatedAt: "2026-01-01T00:00:00Z"},
		{Kind: "commitment", Content: "upcoming", DueAt: tomorrow, CreatedAt: "2026-01-01T00:00:00Z"},
		{Kind: "decision", Content: "d", CreatedAt: "2026-01-01T00:00:00Z"},
		{Kind: "commitment", Content: "no-due", CreatedAt: "2026-01-01T00:00:00Z"},
	}
	sortBriefingOpenItems(items)

	// Overdue commitment must be first; upcoming-due commitment second.
	if items[0].Content != "overdue" {
		t.Errorf("expected overdue commitment first, got %q", items[0].Content)
	}
	if items[1].Content != "upcoming" {
		t.Errorf("expected upcoming commitment second, got %q", items[1].Content)
	}
	_ = today
}

func TestToBriefingHighlight_Truncates(t *testing.T) {
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'x'
	}
	h := toBriefingHighlight(ai.SimilarResult{
		ContentType: "post", ContentUUID: "u1", ContentText: string(long), ChannelName: "design",
	})
	// 160 chars + ellipsis rune. Count runes, not bytes (… is multibyte).
	if len([]rune(h.Snippet)) > 161 {
		t.Errorf("snippet not truncated: %d runes", len([]rune(h.Snippet)))
	}
	if h.ContentType != "post" || h.ContentUUID != "u1" {
		t.Error("highlight projection lost identifiers")
	}
}

// --- backfill liveness / stale detection ---

func TestIsBackfillStale(t *testing.T) {
	now := time.Now().Unix()

	// Fresh heartbeat → live.
	if isBackfillStale(MemoryBackfillStatus{HeartbeatAt: now}) {
		t.Error("a fresh heartbeat must not be considered stale")
	}

	// Old heartbeat → stale (worker died).
	old := time.Now().Add(-5 * time.Minute).Unix()
	if !isBackfillStale(MemoryBackfillStatus{HeartbeatAt: old}) {
		t.Error("a 5-minute-old heartbeat must be considered stale")
	}

	// No heartbeat yet → fall back to StartedAt. Recent start → live.
	if isBackfillStale(MemoryBackfillStatus{StartedAt: now}) {
		t.Error("a just-started job (no heartbeat) must not be stale")
	}

	// No heartbeat, old start → stale (died in its first interval).
	if !isBackfillStale(MemoryBackfillStatus{StartedAt: old}) {
		t.Error("an old start with no heartbeat must be stale")
	}

	// No timing info at all → don't second-guess (not stale).
	if isBackfillStale(MemoryBackfillStatus{}) {
		t.Error("a status with no timing info must not be flagged stale")
	}
}

// makeUserWithNames builds a UserInfo whose channels/projects/DMs carry
// display names + participants, for exercising the scope resolver.
func makeUserWithNames(self uuid.UUID) *userModels.UserInfo {
	return &userModels.UserInfo{
		UserPostgresInfo: userModels.User{Id: self},
		UserDgraphInfo: dgraphStruct.DgraphUser{
			Uuid: self.String(),
			Channels: []*dgraphStruct.DgraphChannel{
				{Uuid: "chan-1", Name: "design"},
			},
			Projects: []*dgraphStruct.DgraphProject{
				{Uuid: "proj-1", Name: "Q3 Launch"},
			},
			DMs: []*dgraphStruct.DgraphDm{
				{
					GroupingId: "groupabcdefghijklmnopqrstuvwxyz12", // 32 chars, no space → group
					Participants: []*dgraphStruct.DgraphUser{
						{Uuid: self.String(), UserFullName: "Me"},
						{Uuid: "u2", UserFullName: "Alice"},
						{Uuid: "u3", UserFullName: "Bob"},
					},
				},
				{
					GroupingId: self.String() + " peeruuid", // space → 1:1 DM
					Participants: []*dgraphStruct.DgraphUser{
						{Uuid: self.String(), UserFullName: "Me"},
						{Uuid: "peeruuid", UserFullName: "Carol"},
					},
				},
			},
		},
	}
}

func TestScopeResolverChannel(t *testing.T) {
	self := uuid.New()
	r := newScopeResolver(makeUserWithNames(self))
	v := adapter.MemoryItemView{ChannelUUID: "chan-1"}
	r.enrich(&v)
	if v.ScopeType != "channel" {
		t.Fatalf("scope_type = %q, want channel", v.ScopeType)
	}
	if v.ChannelName != "design" || v.ScopeLabel != "design" {
		t.Errorf("channel name not resolved: name=%q label=%q", v.ChannelName, v.ScopeLabel)
	}
}

func TestScopeResolverProject(t *testing.T) {
	self := uuid.New()
	r := newScopeResolver(makeUserWithNames(self))
	v := adapter.MemoryItemView{ProjectUUID: "proj-1"}
	r.enrich(&v)
	if v.ScopeType != "project" {
		t.Fatalf("scope_type = %q, want project", v.ScopeType)
	}
	if v.ProjectName != "Q3 Launch" || v.ScopeLabel != "Q3 Launch" {
		t.Errorf("project name not resolved: name=%q label=%q", v.ProjectName, v.ScopeLabel)
	}
}

func TestScopeResolverGroupChat(t *testing.T) {
	self := uuid.New()
	r := newScopeResolver(makeUserWithNames(self))
	v := adapter.MemoryItemView{ChatGrpID: "groupabcdefghijklmnopqrstuvwxyz12"}
	r.enrich(&v)
	if v.ScopeType != "group" {
		t.Fatalf("scope_type = %q, want group", v.ScopeType)
	}
	// Excludes self; lists the other participants.
	if !strings.Contains(v.ScopeLabel, "Alice") || !strings.Contains(v.ScopeLabel, "Bob") {
		t.Errorf("group label missing participants: %q", v.ScopeLabel)
	}
	if strings.Contains(v.ScopeLabel, "Me") {
		t.Errorf("group label should exclude self: %q", v.ScopeLabel)
	}
}

func TestScopeResolverDM(t *testing.T) {
	self := uuid.New()
	r := newScopeResolver(makeUserWithNames(self))
	v := adapter.MemoryItemView{ChatGrpID: self.String() + " peeruuid"}
	r.enrich(&v)
	if v.ScopeType != "dm" {
		t.Fatalf("scope_type = %q, want dm", v.ScopeType)
	}
	if v.ScopeLabel != "Carol" {
		t.Errorf("dm label = %q, want the peer's name 'Carol'", v.ScopeLabel)
	}
}

func TestScopeResolverUnknownScopeNoLeak(t *testing.T) {
	self := uuid.New()
	r := newScopeResolver(makeUserWithNames(self))
	// A channel the user can't see → scope_type set, but NO name leaked.
	v := adapter.MemoryItemView{ChannelUUID: "chan-not-accessible"}
	r.enrich(&v)
	if v.ScopeType != "channel" {
		t.Fatalf("scope_type = %q, want channel", v.ScopeType)
	}
	if v.ChannelName != "" || v.ScopeLabel != "" {
		t.Errorf("must not resolve a name for an inaccessible channel: name=%q label=%q", v.ChannelName, v.ScopeLabel)
	}
}
