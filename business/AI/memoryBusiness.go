package business

// User-facing read/manage operations for the Workspace Memory Layer.
// These power the "what does my workspace know" surface: list items the
// user can see, and manage their lifecycle (resolve / dismiss). All
// retrieval is permission-scoped by the caller's accessible channels and
// projects, mirroring the rest of the app.

import (
	"context"
	"fmt"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/AI"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	memoryModels "github.com/akashc777/OneCamp/models/postgres/WorkspaceMemory"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// ListWorkspaceMemory returns memory items visible to the user, filtered
// by kind/status, plus open-item counts per kind for headline display.
// When scopeChannelUUID is non-empty, results are narrowed to that channel
// — but ONLY if the user can access it, so scoping can never widen access.
func ListWorkspaceMemory(ctx context.Context, userInfo *userModels.UserInfo, kinds, statuses []string, limit int, scopeChannelUUID string) (*adapter.MemoryListResponse, error) {
	channels, projects := getAccessibleResourceUUIDs(userInfo)
	grpIDs := accessibleGroupingIDs(userInfo)
	ownerID := userInfo.UserPostgresInfo.Id

	// Optional channel scoping: intersect with accessible channels so an
	// arbitrary ?channel= param can never reveal items outside the user's
	// access. When the requested channel isn't accessible, we return an
	// empty result rather than the unscoped list.
	scoped := false
	if scopeChannelUUID != "" {
		scoped = true
		if !containsStr(channels, scopeChannelUUID) {
			return &adapter.MemoryListResponse{Items: []adapter.MemoryItemView{}, Counts: map[string]int{}}, nil
		}
		channels = []string{scopeChannelUUID}
	}

	filter := memoryModels.QueryFilter{
		Kinds:              kinds,
		Statuses:           statuses,
		AccessibleChannels: channels,
		AccessibleProjects: projects,
		AccessibleGrpIDs:   grpIDs,
		OwnerID:            &ownerID,
		Limit:              limit,
	}
	countFilter := memoryModels.QueryFilter{
		AccessibleChannels: channels,
		AccessibleProjects: projects,
		AccessibleGrpIDs:   grpIDs,
		OwnerID:            &ownerID,
	}
	// When scoping to a channel, the per-kind counts should reflect that
	// channel only — drop the broad owner/project/grp dimensions so the
	// headline matches the filtered list.
	if scoped {
		filter.AccessibleProjects = nil
		filter.AccessibleGrpIDs = nil
		filter.OwnerID = nil
		countFilter.AccessibleProjects = nil
		countFilter.AccessibleGrpIDs = nil
		countFilter.OwnerID = nil
	}

	items, err := memoryModels.List(ctx, filter)
	if err != nil {
		return nil, err
	}

	counts, err := memoryModels.CountByScope(ctx, countFilter)
	if err != nil {
		// Counts are non-critical; degrade to empty rather than fail the list.
		helpers.LogErrorWithContext(ctx, "memory counts failed: %v", err)
		counts = map[string]int{}
	}

	resp := &adapter.MemoryListResponse{
		Items:  make([]adapter.MemoryItemView, 0, len(items)),
		Counts: counts,
	}
	resolver := newScopeResolver(userInfo)
	for _, it := range items {
		v := toMemoryView(it)
		resolver.enrich(&v)
		resp.Items = append(resp.Items, v)
	}
	return resp, nil
}

// scopeResolver turns a memory item's scope UUIDs into human display names,
// resolved ONLY against the caller's own accessible resources so a name is
// never shown for something the user can't see. Built once per request from
// the cached JWT/Dgraph user info (no extra DB calls), then applied to each
// view. This is the server-side complement to the FE backlink resolver: the
// API is the single source of truth for "where did this come from".
type scopeResolver struct {
	selfUUID     string
	channelNames map[string]string // channel uuid → name
	projectNames map[string]string // project uuid → name
	// dmLabels maps a DM/group grouping id → a display label derived from
	// its participants (peer name for a 1:1 DM, comma-joined names for a
	// group chat). Group ids are 32-char; DM ids are space-joined uuids.
	dmLabels map[string]string
}

// newScopeResolver builds the lookup maps from the user's accessible
// channels, projects, and DM/group memberships.
func newScopeResolver(userInfo *userModels.UserInfo) *scopeResolver {
	r := &scopeResolver{
		selfUUID:     userInfo.UserDgraphInfo.Uuid,
		channelNames: make(map[string]string),
		projectNames: make(map[string]string),
		dmLabels:     make(map[string]string),
	}
	for _, ch := range userInfo.UserDgraphInfo.Channels {
		if ch.Uuid != "" && ch.Name != "" {
			r.channelNames[ch.Uuid] = ch.Name
		}
	}
	for _, pr := range userInfo.UserDgraphInfo.Projects {
		if pr.Uuid != "" && pr.Name != "" {
			r.projectNames[pr.Uuid] = pr.Name
		}
	}
	for _, dm := range userInfo.UserDgraphInfo.DMs {
		if dm.GroupingId == "" {
			continue
		}
		if label := r.participantLabel(dm); label != "" {
			r.dmLabels[dm.GroupingId] = label
		}
	}
	return r
}

// participantLabel renders a conversation's display name from its
// participants, excluding the caller. Falls back to full name, then
// username. Caps a long group list with a "+N" suffix so the label stays
// compact for the UI.
func (r *scopeResolver) participantLabel(dm *dgraphStruct.DgraphDm) string {
	names := make([]string, 0, len(dm.Participants))
	for _, p := range dm.Participants {
		if p == nil || p.Uuid == r.selfUUID {
			continue
		}
		name := strings.TrimSpace(p.UserFullName)
		if name == "" {
			name = strings.TrimSpace(p.UserName)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1, 2, 3:
		return strings.Join(names, ", ")
	default:
		return fmt.Sprintf("%s, %s +%d", names[0], names[1], len(names)-2)
	}
}

// enrich fills the resolved scope display fields on a view, choosing the
// most specific scope present (channel → project → group/DM).
func (r *scopeResolver) enrich(v *adapter.MemoryItemView) {
	switch {
	case v.ChannelUUID != "":
		v.ScopeType = "channel"
		if name := r.channelNames[v.ChannelUUID]; name != "" {
			v.ChannelName = name
			v.ScopeLabel = name
		}
	case v.ProjectUUID != "":
		v.ScopeType = "project"
		if name := r.projectNames[v.ProjectUUID]; name != "" {
			v.ProjectName = name
			v.ScopeLabel = name
		}
	case v.ChatGrpID != "":
		// A 32-char id with no space is a group chat; a space-joined pair
		// is a 1:1 DM. Both resolve their label from participants.
		if strings.Contains(v.ChatGrpID, " ") {
			v.ScopeType = "dm"
		} else {
			v.ScopeType = "group"
		}
		if label := r.dmLabels[v.ChatGrpID]; label != "" {
			v.ScopeLabel = label
		}
	}
}

// CaptureWorkspaceMemory persists a user-initiated memory item captured
// from a message ("save to memory"). Unlike the AI extractor, this is a
// deterministic, high-confidence (100) capture: the user chose the kind
// and the exact text, so there's no LLM call. Access is verified against
// the message's scope so a user can't capture into a channel/group they
// can't see. Linked to the source message via (source_type, source_uuid)
// so the deletion cascade removes it if the message is later deleted.
func CaptureWorkspaceMemory(ctx context.Context, userInfo *userModels.UserInfo, req adapter.CaptureMemoryRequest) (*adapter.MemoryItemView, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	switch kind {
	case memoryModels.KindDecision, memoryModels.KindCommitment, memoryModels.KindQuestion:
	default:
		return nil, fmt.Errorf("invalid kind %q", req.Kind)
	}

	content := strings.TrimSpace(req.Content)
	if content == "" {
		return nil, fmt.Errorf("content is required")
	}
	if len(content) > 1000 {
		content = helpers.TruncateRunes(content, 1000)
	}

	// Gate on the layer being enabled so manual capture doesn't create
	// orphaned items a disabled workspace can't surface.
	settings, err := getAISettingsForMemory(ctx)
	if err != nil {
		return nil, err
	}
	if !settings.enabled || !settings.memoryEnabled {
		return nil, fmt.Errorf("workspace memory is not enabled")
	}

	// Verify scope access — the user must be able to see where the message
	// lives, mirroring the retrieval permission rule.
	scope := MemoryScope{
		SourceType:    strings.TrimSpace(req.SourceType),
		SourceUUID:    strings.TrimSpace(req.SourceUUID),
		CreatedByUUID: userInfo.UserPostgresInfo.Id.String(),
	}
	switch {
	case req.ChannelUUID != "":
		channels, _ := getAccessibleResourceUUIDs(userInfo)
		if !containsStr(channels, req.ChannelUUID) {
			return nil, fmt.Errorf("not authorized for this channel")
		}
		scope.ChannelUUID = req.ChannelUUID
	case req.ChatGrpID != "":
		if !containsStr(accessibleGroupingIDs(userInfo), req.ChatGrpID) {
			return nil, fmt.Errorf("not authorized for this conversation")
		}
		scope.ChatGrpID = req.ChatGrpID
	default:
		return nil, fmt.Errorf("a channel or conversation scope is required")
	}

	// Respect per-scope exclusion: a sensitive channel/group opted out of
	// the memory layer can't be captured into either.
	if scopeIsExcluded(ctx, scope) {
		return nil, fmt.Errorf("workspace memory is disabled for this conversation")
	}

	item := rawExtractedItem{Kind: kind, Content: content, Confidence: 100}
	// A user-supplied due date applies to commitments only (the only kind the
	// overdue nudge + briefing reason about). Validated as YYYY-MM-DD; an
	// invalid value is ignored rather than failing the capture.
	if kind == memoryModels.KindCommitment {
		if d := strings.TrimSpace(req.Due); d != "" {
			if _, err := time.Parse("2006-01-02", d); err == nil {
				item.Due = d
			}
		}
	}
	// Manual capture is a deliberate user action: allow it to revive a fact
	// the user previously deleted (overrides the tombstone guard).
	id, err := persistMemoryItemReturningID(ctx, item, scope, true)
	if err != nil {
		return nil, err
	}

	// Return the freshly created item for optimistic UI rendering.
	created, err := memoryModels.GetByID(ctx, id)
	if err != nil {
		// Persisted fine; just couldn't reload. Synthesize a minimal view.
		return &adapter.MemoryItemView{ID: id.String(), Kind: kind, Content: content, Status: memoryModels.StatusOpen, Confidence: 100}, nil
	}
	v := toMemoryView(created)
	newScopeResolver(userInfo).enrich(&v)
	return &v, nil
}

// containsStr reports whether s is in list.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// UpdateWorkspaceMemoryStatus changes an item's status after verifying the
// caller can see it (same permission rule as listing).
func UpdateWorkspaceMemoryStatus(ctx context.Context, userInfo *userModels.UserInfo, id uuid.UUID, status string) error {
	switch status {
	case memoryModels.StatusOpen, memoryModels.StatusResolved,
		memoryModels.StatusSuperseded, memoryModels.StatusDismissed:
	default:
		return fmt.Errorf("invalid status %q", status)
	}
	if err := authorizeMemoryAccess(ctx, userInfo, id); err != nil {
		return err
	}
	if err := memoryModels.UpdateStatus(ctx, id, status); err != nil {
		return err
	}
	// Keep the GraphRAG projection's status in sync (best-effort). A
	// resolved/dismissed item should stop surfacing in "open items" graph
	// reads. Async so the user's action isn't coupled to graph latency.
	updateGraphMemoryStatusAsync(id.String(), status)
	// Close the nudge loop: the moment an item leaves the OPEN state, clear any
	// open nudge pointing at it so the bell doesn't keep showing a reminder for
	// something the user just handled (instead of waiting for the next sweep).
	if status != memoryModels.StatusOpen {
		go ClearNudgeForMemoryItem(context.WithoutCancel(ctx), id.String())
	}
	return nil
}

// UpdateWorkspaceMemoryDue sets or clears a commitment's due date after
// verifying the caller can see it. due is "YYYY-MM-DD", or "" to clear. Only
// commitments carry a due date; other kinds are rejected so the field stays
// meaningful (and the overdue nudge only reasons about commitments).
func UpdateWorkspaceMemoryDue(ctx context.Context, userInfo *userModels.UserInfo, id uuid.UUID, due string) error {
	if err := authorizeMemoryAccess(ctx, userInfo, id); err != nil {
		return err
	}

	item, err := memoryModels.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("memory item not found")
	}
	if item.Kind != memoryModels.KindCommitment {
		return fmt.Errorf("only commitments can have a due date")
	}

	var dueAt *time.Time
	if d := strings.TrimSpace(due); d != "" {
		t, perr := time.Parse("2006-01-02", d)
		if perr != nil {
			return fmt.Errorf("invalid due date, expected YYYY-MM-DD")
		}
		dueAt = &t
	}
	return memoryModels.UpdateDueDate(ctx, id, dueAt)
}

// DeleteWorkspaceMemory soft-deletes an item the caller can see.
func DeleteWorkspaceMemory(ctx context.Context, userInfo *userModels.UserInfo, id uuid.UUID) error {
	if err := authorizeMemoryAccess(ctx, userInfo, id); err != nil {
		return err
	}
	if err := memoryModels.SoftDelete(ctx, id); err != nil {
		return err
	}
	// Drop the projections so a dismissed item can't resurface via semantic
	// recall or graph reads. Best-effort, async.
	ai.DeleteMemoryProjectionsAsync(id.String())
	// A deleted commitment must also clear any open nudge for it immediately.
	go ClearNudgeForMemoryItem(context.WithoutCancel(ctx), id.String())
	return nil
}

// authorizeMemoryAccess returns nil iff the item exists and is within the
// user's permission scope (accessible channel/project, owned, or created).
func authorizeMemoryAccess(ctx context.Context, userInfo *userModels.UserInfo, id uuid.UUID) error {
	item, err := memoryModels.GetByID(ctx, id)
	if err != nil {
		return err
	}
	if memoryItemVisibleTo(item, userInfo) {
		return nil
	}
	return fmt.Errorf("not authorized for this memory item")
}

// memoryItemVisibleTo decides whether a user may READ/ACT on a single item.
//
// PRIVACY INVARIANT: visibility is gated on CURRENT membership of the item's
// scope (channel / project / DM-group), NOT on ownership or authorship. Every
// item is created with a scope the owner/creator was a member of at the time,
// but membership changes — a user who leaves a channel must immediately lose
// the ability to read or act on that channel's commitments, even ones they
// own or wrote. Granting access on a bare OwnerID/CreatedBy match would let a
// stale claim follow the user out of the scope and leak the (possibly evolving)
// content. So those are deliberately NOT shortcuts here; the user must still be
// in the scope, in which case the scope checks below return true anyway.
func memoryItemVisibleTo(item *memoryModels.MemoryItem, userInfo *userModels.UserInfo) bool {
	if item.ChannelUUID != nil {
		for _, ch := range userInfo.UserDgraphInfo.Channels {
			if ch.Uuid == item.ChannelUUID.String() {
				return true
			}
		}
	}
	if item.ProjectUUID != nil {
		for _, pr := range userInfo.UserDgraphInfo.Projects {
			if pr.Uuid == item.ProjectUUID.String() {
				return true
			}
		}
	}
	// DM / group-chat scope: visible to any participant of that grouping.
	if item.ChatGrpID != "" {
		for _, dm := range userInfo.UserDgraphInfo.DMs {
			if dm.GroupingId == item.ChatGrpID {
				return true
			}
		}
	}
	return false
}

func toMemoryView(it *memoryModels.MemoryItem) adapter.MemoryItemView {
	v := adapter.MemoryItemView{
		ID:         it.ID.String(),
		Kind:       it.Kind,
		Content:    it.Content,
		Status:     it.Status,
		SourceType: it.SourceType,
		SourceUUID: it.SourceUUID,
		ChatGrpID:  it.ChatGrpID,
		Confidence: it.Confidence,
		CreatedAt:  it.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:  it.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if it.OwnerID != nil {
		v.OwnerID = it.OwnerID.String()
	}
	if it.DueAt != nil {
		v.DueAt = it.DueAt.Format("2006-01-02")
	}
	if it.ChannelUUID != nil {
		v.ChannelUUID = it.ChannelUUID.String()
	}
	if it.ProjectUUID != nil {
		v.ProjectUUID = it.ProjectUUID.String()
	}
	return v
}

// SetMemoryLayerEnabled toggles the memory extraction agent (admin).
func SetMemoryLayerEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetMemoryLayerEnabled(ctx, enabled)
}

// SetTeamReportEnabled toggles the weekly team-report agent (admin).
func SetTeamReportEnabled(ctx context.Context, enabled bool) error {
	return aiModels.SetTeamReportEnabled(ctx, enabled)
}

// GetChannelMemoryExcluded reports whether a channel is opted out of the
// memory layer. The caller must be able to see the channel.
func GetChannelMemoryExcluded(ctx context.Context, userInfo *userModels.UserInfo, channelUUID string) (bool, error) {
	if !channelAccessOrVerify(ctx, userInfo, channelUUID) {
		return false, fmt.Errorf("not authorized for this channel")
	}
	return memoryModels.IsScopeExcluded(ctx, memoryModels.ExclusionChannel, channelUUID)
}

// SetChannelMemoryExcluded opts a channel in/out of the memory layer. Only a
// channel admin may change it — this is a workspace-trust control, not a
// per-member preference.
func SetChannelMemoryExcluded(ctx context.Context, userInfo *userModels.UserInfo, channelUUID string, excluded bool) error {
	dgraphChannel, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphChannel == nil {
		return fmt.Errorf("channel not found or not authorized")
	}
	if dgraphChannel.IsAdmin == 0 {
		return fmt.Errorf("you must be a channel admin to change this")
	}
	uid := userInfo.UserPostgresInfo.Id
	return memoryModels.SetScopeExcluded(ctx, memoryModels.ExclusionChannel, channelUUID, &uid, excluded)
}

// channelAccessOrVerify checks the cached JWT channel list first, then
// falls back to a Dgraph query. Newly-created channels won't be in the
// cached list until the user refreshes their token.
func channelAccessOrVerify(ctx context.Context, userInfo *userModels.UserInfo, channelUUID string) bool {
	if containsStr(channelAccessUUIDs(userInfo), channelUUID) {
		return true
	}
	// Fallback: verify via Dgraph in case the channel was just created.
	dgraphChannel, err := channelDomain.GetDgraphChannelInfoByUUID(ctx, channelUUID, userInfo.UserDgraphInfo.Uid)
	if err != nil || dgraphChannel == nil {
		return false
	}
	return dgraphChannel.IsMember == 1 || dgraphChannel.IsAdmin == 1
}

// channelAccessUUIDs returns the channel uuids the user can see.
func channelAccessUUIDs(userInfo *userModels.UserInfo) []string {
	out := make([]string, 0, len(userInfo.UserDgraphInfo.Channels))
	for _, ch := range userInfo.UserDgraphInfo.Channels {
		if ch.Uuid != "" {
			out = append(out, ch.Uuid)
		}
	}
	return out
}
