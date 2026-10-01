package business

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// DMResolveStats summarises the DM/MPIM pre-pass for the FE plan view.
// Note that DMs and MPIMs don't get one-OneCamp-channel-per-Slack-DM —
// instead they're stored as chats in OneCamp's grouping system. The
// "creation" we do here is the per-DM grouping_id calculation; messages
// are inserted into those groupings by the message worker.
type DMResolveStats struct {
	DMs                   int `json:"dms"`
	MPIMs                 int `json:"mpims"`
	SkippedNoMembers      int `json:"skipped_no_members"`
	SkippedUnmappedMember int `json:"skipped_unmapped_member"`
}

// resolveDMs walks the dms.json + mpims.json lists and stamps each
// Slack DM/MPIM id into the per-import id_map with the OneCamp grouping
// id that DM messages will be inserted into. The grouping id is a
// deterministic hash of the participant UUIDs (helpers.GenerateGroupID),
// so re-imports of the same workspace produce the same grouping —
// crucial for Tier 2 dedup to work.
//
// Side effect: zero. We don't write `chats` rows here; the message
// worker creates those per imported message via the existing CreateChat
// / CreateChatForGroup paths. This keeps the import implementation
// small and reuses every code path that was already exercised by
// hand-typed chats in production.
//
// Membership: DMs require both members to be resolved. If even one
// member is missing from the user id_map we skip with a warning. MPIMs
// require at least the importing admin to be a member — Slack always
// includes them when the export was generated under their token.
func resolveDMs(ctx context.Context, importId uuid.UUID, workspaceName string,
	dms, mpims []SlackChannel, importingUser *userModels.UserInfo) (DMResolveStats, error) {

	stats := DMResolveStats{}

	// Build the user id_map up front in one batched query rather than
	// hammering PG with one lookup per member. Slack DMs typically have
	// 2 members; MPIMs up to 8.
	allMemberSlackIds := make(map[string]struct{}, 64)
	for _, d := range dms {
		for _, m := range d.Members {
			allMemberSlackIds[m] = struct{}{}
		}
	}
	for _, m := range mpims {
		for _, mem := range m.Members {
			allMemberSlackIds[mem] = struct{}{}
		}
	}
	memberIds := make([]string, 0, len(allMemberSlackIds))
	for id := range allMemberSlackIds {
		memberIds = append(memberIds, id)
	}
	sort.Strings(memberIds) // determinism for tests/debug
	idMap, err := importModels.LookupIdMappingsBatch(ctx, importId, importModels.EntityUser, memberIds)
	if err != nil {
		return stats, fmt.Errorf("dm member lookup: %w", err)
	}

	// Process DMs.
	for i := range dms {
		dm := &dms[i]
		if dm.ID == "" || len(dm.Members) < 2 {
			stats.SkippedNoMembers++
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityDM, dm.ID,
				importModels.SeverityWarning, "DM_NO_MEMBERS",
				"DM has fewer than 2 members; skipping",
				mustMarshal(map[string]interface{}{"members": dm.Members}))
			continue
		}
		// Defensive: very old exports occasionally placed multi-party
		// IMs in dms.json. Route those through the MPIM grouping
		// (SHA-256 hash of all member uuids) instead of the 1:1 sorted
		// pair. The chunk channel-id matches the slack DM id either
		// way; the dm worker reads the metadata blob to know whether
		// to use CreateChat or CreateChatForGroup.
		isMPIMShapedDM := len(dm.Members) > 2
		// Tier 1: same-import retry idempotency.
		if existing, _ := importModels.LookupIdMapping(ctx, importId, importModels.EntityDM, dm.ID); existing != uuid.Nil {
			continue
		}
		// Tier 2: prior import of same workspace.
		if existing, _ := importModels.LookupWorkspaceMapping(ctx, workspaceName, importModels.EntityDM, dm.ID); existing != uuid.Nil {
			_ = importModels.UpsertIdMappingWithOwnership(ctx, importId,
				importModels.EntityDM, dm.ID, existing, nil,
				mustMarshal(map[string]interface{}{"matched_by": "workspace_map"}),
				false)
			continue
		}

		// Resolve both members.
		memberOcUUIDs := make([]string, 0, 2)
		allResolved := true
		for _, sm := range dm.Members {
			if u, ok := idMap[sm]; ok {
				memberOcUUIDs = append(memberOcUUIDs, u.String())
			} else {
				allResolved = false
				break
			}
		}
		if !allResolved {
			stats.SkippedUnmappedMember++
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityDM, dm.ID,
				importModels.SeverityWarning, "DM_UNMAPPED_MEMBER",
				"DM skipped: at least one member could not be mapped to a OneCamp user",
				mustMarshal(map[string]interface{}{"members": dm.Members}))
			continue
		}

		// helpers.GetGroupingId handles the canonical 1:1 grouping.
		// For the rare 3+ "DM" shape (legacy exports), use the same
		// SHA-256 path that genuine MPIMs use so worker logic branches
		// correctly later.
		var groupingId string
		if isMPIMShapedDM {
			id, err := helpers.GenerateGroupID(memberOcUUIDs)
			if err != nil {
				importModels.LogImportError(ctx, importId, nil,
					importModels.EntityDM, dm.ID,
					importModels.SeverityWarning, "DM_GROUPING_FAILED",
					fmt.Sprintf("could not derive grouping id: %v", err), nil)
				continue
			}
			groupingId = id
		} else {
			groupingId = helpers.GetGroupingId(memberOcUUIDs[0], memberOcUUIDs[1])
		}

		// We store the DM mapping with a deterministic placeholder UUID
		// (the column is NOT NULL); the grouping_id goes into metadata
		// since it's not a UUID. Workers consult the metadata blob for
		// the right grouping when inserting messages. Using a SHA1 of
		// the grouping id makes the placeholder stable across re-runs
		// so the workspace mapping table doesn't churn.
		ph := uuid.NewSHA1(uuid.Nil, []byte("dm:"+groupingId))
		if err := importModels.UpsertIdMappingWithOwnership(ctx, importId,
			importModels.EntityDM, dm.ID, ph, nil,
			mustMarshal(map[string]interface{}{
				"grouping_id":   groupingId,
				"members":       memberOcUUIDs,
				"slack_members": dm.Members,
				"is_mpim":       isMPIMShapedDM,
			}), true); err != nil {
			return stats, err
		}
		_ = importModels.UpsertWorkspaceMapping(ctx, workspaceName,
			importModels.EntityDM, dm.ID, ph, importId)
		stats.DMs++
	}

	// Process MPIMs.
	for i := range mpims {
		mp := &mpims[i]
		if mp.ID == "" || len(mp.Members) < 3 {
			// MPIMs are by definition 3+ members. <3 = malformed / single-DM mislabel.
			stats.SkippedNoMembers++
			continue
		}
		if existing, _ := importModels.LookupIdMapping(ctx, importId, importModels.EntityMPIM, mp.ID); existing != uuid.Nil {
			continue
		}
		if existing, _ := importModels.LookupWorkspaceMapping(ctx, workspaceName, importModels.EntityMPIM, mp.ID); existing != uuid.Nil {
			_ = importModels.UpsertIdMappingWithOwnership(ctx, importId,
				importModels.EntityMPIM, mp.ID, existing, nil,
				mustMarshal(map[string]interface{}{"matched_by": "workspace_map"}),
				false)
			continue
		}

		// Resolve all members. Drop missing ones (MPIM tolerates partial
		// resolution because the importing admin needs to be present;
		// extra unmapped members get a warning per missing-member).
		memberOcUUIDs := make([]string, 0, len(mp.Members))
		var unmapped []string
		for _, sm := range mp.Members {
			if u, ok := idMap[sm]; ok {
				memberOcUUIDs = append(memberOcUUIDs, u.String())
			} else {
				unmapped = append(unmapped, sm)
			}
		}
		if len(memberOcUUIDs) < 3 {
			stats.SkippedUnmappedMember++
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityMPIM, mp.ID,
				importModels.SeverityWarning, "MPIM_UNMAPPED_MEMBERS",
				"MPIM skipped: not enough members could be mapped",
				mustMarshal(map[string]interface{}{"unmapped": unmapped}))
			continue
		}
		if len(unmapped) > 0 {
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityMPIM, mp.ID,
				importModels.SeverityWarning, "MPIM_PARTIAL_MEMBERS",
				fmt.Sprintf("MPIM imported with %d/%d members (others unmapped)",
					len(memberOcUUIDs), len(mp.Members)),
				mustMarshal(map[string]interface{}{"unmapped": unmapped}))
		}

		groupingId, err := helpers.GenerateGroupID(memberOcUUIDs)
		if err != nil {
			importModels.LogImportError(ctx, importId, nil,
				importModels.EntityMPIM, mp.ID,
				importModels.SeverityError, "MPIM_GROUPING_FAILED",
				err.Error(), nil)
			continue
		}
		ph := uuid.NewSHA1(uuid.Nil, []byte("mpim:"+groupingId))
		if err := importModels.UpsertIdMappingWithOwnership(ctx, importId,
			importModels.EntityMPIM, mp.ID, ph, nil,
			mustMarshal(map[string]interface{}{
				"grouping_id":   groupingId,
				"members":       memberOcUUIDs,
				"slack_members": mp.Members,
				"is_mpim":       true,
			}), true); err != nil {
			return stats, err
		}
		_ = importModels.UpsertWorkspaceMapping(ctx, workspaceName,
			importModels.EntityMPIM, mp.ID, ph, importId)
		stats.MPIMs++
	}

	patch := mustMarshal(map[string]interface{}{"dms": stats})
	_ = importModels.UpdateProgress(ctx, importId, patch)
	return stats, nil
}

// dmContext is the per-chunk scratch space for a DM/MPIM message worker.
// Mirrors messageContext but for the chats path; kept separate so the
// post-vs-chat side effects don't bleed into one combined struct.
type dmContext struct {
	importId      uuid.UUID
	chunkId       uuid.UUID
	chunkType     string
	workspaceName string

	slackId     string
	groupingId  string
	memberUUIDs []string // OneCamp user UUIDs
	isMPIM      bool

	// User caches mirror messageContext.
	userCache     map[string]*userModels.UserInfo
	userNameCache map[string]string

	// Dgraph DM-node materialisation flag. False on the first message
	// of this MPIM — that call into CreateChatForGroup must leave
	// chatInfo.GrpUuid empty so the existing code path writes the DM
	// node + participants edges. Subsequent messages flip this to true
	// and pass the precomputed grouping_id to skip the edge write.
	//
	// For 1:1 DMs the flag is unused: CreateChat unconditionally writes
	// the DM node every time (it derives the grouping internally).
	dmNodeMaterialised bool

	// Resume cursor.
	lastCursor string
	itemsDone  int

	// File IDs we've already enqueued during this chunk. Same role as
	// messageContext.enqueuedFiles — prevents the per-message file
	// loop from issuing duplicate inserts when pass 2 sees a thread
	// reply that references the same file as its parent.
	enqueuedFiles map[string]struct{}

	// Pending file-chunk inserts buffered for batch flush at chunk end.
	pendingFileChunks []*importModels.Chunk
}

// loadDMContext reads the metadata stored at DM resolution time and
// hydrates a dmContext. Called per chunk so workers don't re-parse the
// metadata blob per message.
func loadDMContext(ctx context.Context, chunk *importModels.Chunk, workspaceName string) (*dmContext, error) {
	if chunk.ChannelSlackId == nil {
		return nil, fmt.Errorf("dm chunk missing slack_id")
	}
	slackId := *chunk.ChannelSlackId

	// We saved the grouping_id + members in id_map metadata. Pull it back.
	md, err := importModels.GetIdMapMetadata(ctx, chunk.ImportId, importModels.EntityDM, slackId)
	if err != nil || len(md) == 0 {
		// Fall back to MPIM lookup.
		md, err = importModels.GetIdMapMetadata(ctx, chunk.ImportId, importModels.EntityMPIM, slackId)
	}
	if err != nil || len(md) == 0 {
		return nil, fmt.Errorf("dm metadata not found: %s", slackId)
	}

	var meta struct {
		GroupingId string   `json:"grouping_id"`
		Members    []string `json:"members"`
		IsMPIM     bool     `json:"is_mpim"`
	}
	if err := jsonUnmarshalRaw(md, &meta); err != nil {
		return nil, fmt.Errorf("dm metadata parse: %w", err)
	}

	dc := &dmContext{
		importId:      chunk.ImportId,
		chunkId:       chunk.Id,
		chunkType:     chunk.ChunkType,
		workspaceName: workspaceName,
		slackId:       slackId,
		groupingId:    meta.GroupingId,
		memberUUIDs:   meta.Members,
		isMPIM:        meta.IsMPIM,
		userCache:     make(map[string]*userModels.UserInfo, 8),
		userNameCache: make(map[string]string, 8),
		enqueuedFiles: make(map[string]struct{}, 16),
	}

	// Detect whether the Dgraph DM node for this MPIM already exists.
	// Two paths to true:
	//   1. A prior chunk in this same import already wrote a message
	//      for this DM (per-import resume / re-run).
	//   2. A prior import of the same workspace wrote the DM
	//      (cross-import dedup; the workspace_id_map covers this).
	//
	// We check both because either implies the Dgraph DM node exists
	// and `chatInfo.GrpUuid` should be set so CreateChatForGroup
	// short-circuits its DM-node write.
	if dc.isMPIM {
		owned, _ := importModels.HasAnyOwnedMessageForDM(ctx, chunk.ImportId, slackId)
		if owned {
			dc.dmNodeMaterialised = true
		} else if workspaceName != "" {
			// Workspace map check — if a prior import wrote this MPIM,
			// the DM node still exists in Dgraph.
			if existing, _ := importModels.LookupWorkspaceMapping(ctx, workspaceName, importModels.EntityMPIM, slackId); existing != uuid.Nil {
				dc.dmNodeMaterialised = true
			}
		}
	}

	return dc, nil
}

// dmResolveAuthor mirrors messageContext.resolveAuthor for the chat path.
func (dc *dmContext) resolveAuthor(ctx context.Context, m *SlackMessage, importingUser *userModels.UserInfo) (*userModels.UserInfo, *dgraphStruct.DgraphUser) {
	authorId := m.User
	if authorId == "" && m.BotID != "" {
		return importingUser, &importingUser.UserDgraphInfo
	}
	if authorId == "" {
		return importingUser, &importingUser.UserDgraphInfo
	}
	if cached, ok := dc.userCache[authorId]; ok {
		return cached, &cached.UserDgraphInfo
	}
	ocUUID, _ := importModels.LookupIdMapping(ctx, dc.importId, importModels.EntityUser, authorId)
	if ocUUID == uuid.Nil {
		return importingUser, &importingUser.UserDgraphInfo
	}
	pgUser, err := userBusiness.GetUserByUUID(ctx, ocUUID)
	if err != nil || pgUser == nil {
		return importingUser, &importingUser.UserDgraphInfo
	}
	dgUser, err := userBusiness.GetDgraphUserInfoByUUID(ctx, ocUUID.String())
	if err != nil || dgUser == nil {
		return importingUser, &importingUser.UserDgraphInfo
	}
	info := &userModels.UserInfo{UserPostgresInfo: *pgUser, UserDgraphInfo: *dgUser}
	dc.userCache[authorId] = info
	return info, dgUser
}

// jsonUnmarshalRaw is a tiny wrapper used by loadDMContext to keep the
// metadata-shape struct local to that function while still benefiting
// from a shared decode helper.
func jsonUnmarshalRaw(raw []byte, v interface{}) error {
	return json.Unmarshal(raw, v)
}
