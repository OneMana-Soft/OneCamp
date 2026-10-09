package business

import (
	"context"
	"errors"
	"strings"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Board"
	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	resourceViewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	domain "github.com/akashc777/OneCamp/domain/Board"
	commentDomain "github.com/akashc777/OneCamp/domain/Comment"
	globalSearchDomain "github.com/akashc777/OneCamp/domain/GlobalSearch"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphActivityModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	openSearchBoardModels "github.com/akashc777/OneCamp/models/openSearch/Board"
	"github.com/google/uuid"
)

// boardCommentNamespace seeds the deterministic UUID derived from a board's id
// and a Yjs comment id, so the server-side mirror of a board comment keeps a
// stable primary key across create/update/delete (the canvas comment id is not
// itself a UUID).
var boardCommentNamespace = uuid.MustParse("b0a4d1e2-0000-4000-8000-000000000001")

// boardCommentUUID maps a (board, Yjs comment id) pair to a stable UUID used as
// the mirror's id in Postgres, Dgraph, OpenSearch and the embedding index.
func boardCommentUUID(boardUUID, commentID string) uuid.UUID {
	return uuid.NewSHA1(boardCommentNamespace, []byte(boardUUID+":"+commentID))
}

// CreateBoard creates a board owned by createdByUser, who is also added as an
// editor (so the creator has edit access). Mirrors business/Doc.CreateDoc but
// without OpenSearch/AI-embed (boards are not text-indexed in v1).
func CreateBoard(ctx context.Context, createdByUser *dgraphStruct.DgraphUser, input *adapter.InputCreateBoard) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	newBoardUUID := uuid.New()
	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	boolFalse := false
	isPrivate := boolFalse
	if input.BoardPrivate {
		isPrivate = true
	}

	title := input.BoardTitle
	if title == "" {
		title = "Untitled board"
	}

	dgraphBoard = &dgraphStruct.DgraphBoard{
		DType: []string{"Board"},
		Uuid:  newBoardUUID.String(),
		Title: title,
		CreatedBy: &dgraphStruct.DgraphUser{
			Uid: createdByUser.Uid,
		},
		IsPrivate: &isPrivate,
		EditingUser: []*dgraphStruct.DgraphUser{{
			Uid: createdByUser.Uid,
		}},
		CreatedAt: &currentTime,
		UpdatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}

	_, err = domain.CreateOrUpdateDgraphBoard(ctx, dgraphBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/CreateBoard Failed to create board in dgraph err: %+v", err)
		return
	}

	// Index in OpenSearch for global search (async, best-effort). The creator
	// is also the sole editor at creation time.
	go openSearchBoardModels.CreateBoardInOpenSearch(context.Background(), &openSearchStruct.OpenSearchBoard{
		Uuid:                   newBoardUUID.String(),
		BoardTitle:             title,
		BoardCreatedByUserUuid: createdByUser.Uuid,
		BoardCreatedByFullName: createdByUser.UserFullName,
		BoardCreatedByProfile:  createdByUser.ProfileKey,
		BoardPrivate:           isPrivate,
		BoardEditingUsers:      []string{createdByUser.Uuid},
		BoardCreatedAt:         currentTime.Unix(),
		BoardUpdatedAt:         currentTime.Unix(),
	})

	return
}

// GetBoardByBoardUUID returns the board with the caller's access for opening it.
func GetBoardByBoardUUID(ctx context.Context, boardUUID string, userDgraphUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	dgraphBoard, err = domain.GetDgraphBoardByUUID(ctx, boardUUID, userDgraphUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBoardByBoardUUID Failed to get board in dgraph err: %+v", err)
		return
	}
	return
}

// GetBasicBoardByUUID returns minimal access/ownership info for authorization.
func GetBasicBoardByUUID(ctx context.Context, boardUUID string, userDgraphUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	return domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, userDgraphUID)
}

// GetSystemBoardByUUID returns board state for the collaboration service. The
// canvas blob is resolved from object storage (board_state_key) when present,
// falling back to the legacy inline board_state for un-migrated boards.
func GetSystemBoardByUUID(ctx context.Context, boardUUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	dgraphBoard, err = domain.GetSystemBoardByUUID(ctx, boardUUID)
	if err != nil {
		return
	}
	resolveBoardState(ctx, dgraphBoard)
	return
}

// UpdateBoard updates board metadata (title / privacy). Bumps board_updated_at.
func UpdateBoard(ctx context.Context, input *adapter.InputUpdateBoard) (err error) {
	dgraphBoard := &dgraphStruct.DgraphBoard{
		Uid:  "uid(board)",
		Uuid: input.BoardId,
	}
	if input.Title != nil {
		dgraphBoard.Title = *input.Title
	}
	if input.IsPrivate != nil {
		dgraphBoard.IsPrivate = input.IsPrivate
	}
	if input.ThumbnailKey != nil {
		dgraphBoard.ThumbnailKey = *input.ThumbnailKey
	}
	// Bump updated_at only for real edits, not for periodic thumbnail refreshes
	// (which would otherwise constantly reorder the recents list).
	if input.Title != nil || input.IsPrivate != nil {
		currentTime := time.Now()
		dgraphBoard.UpdatedAt = &currentTime
	}

	_, err = domain.CreateOrUpdateDgraphBoard(ctx, dgraphBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/UpdateBoard Failed to update board in dgraph err: %+v", err)
		return
	}

	// Keep the search index in sync for title/privacy changes (map update so a
	// false privacy boolean is written, not dropped).
	if input.Title != nil || input.IsPrivate != nil {
		fields := map[string]interface{}{"updated_date": time.Now().Unix()}
		if input.Title != nil {
			fields["board_title"] = *input.Title
		}
		if input.IsPrivate != nil {
			fields["board_private"] = *input.IsPrivate
		}
		go openSearchBoardModels.UpdateBoardFieldsInOpenSearch(context.Background(), input.BoardId, fields)
	}
	return
}

// UpdateBoardFromCollab persists the serialized Yjs board state + snippet from
// the collaboration service, bumping board_updated_at. Idempotent upsert.
func UpdateBoardFromCollab(ctx context.Context, input *adapter.InputBoardCollabUpdate) (err error) {
	newBytes := len(input.State)

	// Only read the (potentially large) prior state when a mass-deletion is
	// plausible. The previous size is cached cheaply in Redis, so a normal edit
	// skips the full-state read entirely. On a cache miss (first save, expiry,
	// or Redis unavailable) we fall back to reading it so detection is never
	// silently lost.
	needOld := true
	if cached, ok := resourceViewBusiness.CachedStateSize(ctx, resourceViewBusiness.ResourceBoard, input.BoardUuid); ok {
		needOld = cached >= snapshotMassDeleteMinBytes && float64(newBytes) < float64(cached)*snapshotMassDeleteRatio
	}

	var oldState, oldSnippet string
	if needOld {
		if prev, perr := GetSystemBoardByUUID(ctx, input.BoardUuid); perr == nil && prev != nil {
			oldState = prev.State
			oldSnippet = prev.Snippet
		}
	}

	// Persist the canvas blob to object storage (MinIO) and keep only a pointer
	// in dgraph, so a board's growing Yjs state never bloats the graph DB or
	// trips gRPC message limits. Fall back to an inline write if object storage
	// is unavailable, so a save is never lost.
	if key, uerr := putLiveBoardState(ctx, input.BoardUuid, input.State); uerr == nil {
		if err = domain.SetBoardStateRef(ctx, input.BoardUuid, key, input.Snippet); err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateBoardFromCollab Failed to persist board state ref err: %+v", err)
			return
		}
	} else {
		helpers.LogErrorWithContext(ctx, "business/UpdateBoardFromCollab object-storage write failed, falling back to inline err: %+v", uerr)
		currentTime := time.Now()
		dgraphBoard := &dgraphStruct.DgraphBoard{
			Uid:       "uid(board)",
			Uuid:      input.BoardUuid,
			State:     input.State,
			Snippet:   input.Snippet,
			UpdatedAt: &currentTime,
		}
		if _, err = domain.CreateOrUpdateDgraphBoard(ctx, dgraphBoard); err != nil {
			helpers.LogErrorWithContext(ctx, "business/UpdateBoardFromCollab Failed to persist board state err: %+v", err)
			return
		}
	}

	// Cache the new size for the next save's cheap shrink check.
	resourceViewBusiness.SetCachedStateSize(ctx, resourceViewBusiness.ResourceBoard, input.BoardUuid, newBytes)

	// Evaluate/capture a snapshot off the persist path (version history +
	// mass-delete recovery). Never blocks or fails the persist.
	go MaybeSnapshotBoard(context.Background(), input.BoardUuid, oldState, oldSnippet, input.State, input.Snippet, input.Contributors)
	return
}

// DeleteBoard soft-deletes a board (sets board_deleted_at).
func DeleteBoard(ctx context.Context, boardUUID string) (err error) {
	currentTime := time.Now()
	dgraphBoard := &dgraphStruct.DgraphBoard{
		DType:     []string{"Board"},
		Uid:       "uid(board)",
		Uuid:      boardUUID,
		DeletedAt: &currentTime,
	}

	_, err = domain.CreateOrUpdateDgraphBoard(ctx, dgraphBoard)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteBoard Failed to delete board in dgraph err: %+v", err)
		return
	}
	go openSearchBoardModels.DeleteBoardInOpenSearch(context.Background(), boardUUID, currentTime.Unix())

	// Cascade the delete to board comments mirrored for search + AI so they stop
	// surfacing once the board is gone. The Dgraph comments drop from activity
	// automatically via the board_deleted_at filter on the comment_board edge.
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"comment_board_id"},
		boardUUID,
		currentTime.Unix(),
		[]string{"comments"},
		"cascade",
	)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"board_uuid"},
		boardUUID,
		currentTime.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)
	return
}

// UpdateBoardPermissions adds/removes collaborators. Owner only. Enforces a
// single role per user (adding to one role removes them from the others).
func UpdateBoardPermissions(ctx context.Context, input adapter.InputUpdateBoardPermissions, userUID string) (err error) {
	checkBoard, err := domain.GetBasicDgraphBoardByUUID(ctx, input.BoardId, userUID)
	if err != nil {
		return err
	}
	if checkBoard == nil {
		return errors.New("board not found")
	}
	if checkBoard.CreatedBy == nil || checkBoard.CreatedBy.Uid != userUID {
		return errors.New("unauthorized: only the board owner can manage permissions")
	}

	ensureUnique := func(slice []string) []string {
		keys := make(map[string]bool)
		list := []string{}
		for _, entry := range slice {
			if _, value := keys[entry]; !value {
				keys[entry] = true
				list = append(list, entry)
			}
		}
		return list
	}

	if len(input.AddEditors) > 0 {
		input.RemoveViewers = append(input.RemoveViewers, input.AddEditors...)
		input.RemoveCommenters = append(input.RemoveCommenters, input.AddEditors...)
	}
	if len(input.AddViewers) > 0 {
		input.RemoveEditors = append(input.RemoveEditors, input.AddViewers...)
		input.RemoveCommenters = append(input.RemoveCommenters, input.AddViewers...)
	}
	if len(input.AddCommenters) > 0 {
		input.RemoveEditors = append(input.RemoveEditors, input.AddCommenters...)
		input.RemoveViewers = append(input.RemoveViewers, input.AddCommenters...)
	}

	input.RemoveEditors = ensureUnique(input.RemoveEditors)
	input.RemoveViewers = ensureUnique(input.RemoveViewers)
	input.RemoveCommenters = ensureUnique(input.RemoveCommenters)

	if err := domain.UpdateBoardPermissions(ctx, &input); err != nil {
		return err
	}

	// Sync the search index's access lists so shared boards are findable by the
	// right people (and removed users stop seeing them in search).
	go func() {
		perms, perr := domain.GetBoardPermissions(context.Background(), input.BoardId)
		if perr != nil || perms == nil {
			return
		}
		readers := make([]string, 0, len(perms.ReadingUser))
		for _, u := range perms.ReadingUser {
			readers = append(readers, u.Uuid)
		}
		editors := make([]string, 0, len(perms.EditingUser))
		for _, u := range perms.EditingUser {
			editors = append(editors, u.Uuid)
		}
		commenters := make([]string, 0, len(perms.CommentingUser))
		for _, u := range perms.CommentingUser {
			commenters = append(commenters, u.Uuid)
		}
		openSearchBoardModels.UpdateBoardFieldsInOpenSearch(context.Background(), input.BoardId, map[string]interface{}{
			"board_reading_users":    readers,
			"board_editing_users":    editors,
			"board_commenting_users": commenters,
		})
	}()
	return nil
}

// GetBoardPermissions returns collaborators for the sharing UI. Requires edit
// access or ownership.
func GetBoardPermissions(ctx context.Context, boardUUID string, userUID string) (dgraphBoard *dgraphStruct.DgraphBoard, err error) {
	basic, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, userUID)
	if err != nil {
		return nil, err
	}
	if basic == nil {
		return nil, errors.New("board not found")
	}
	isOwner := basic.CreatedBy != nil && basic.CreatedBy.Uid == userUID
	if !isOwner && basic.HasEditAccess == 0 {
		return nil, errors.New("unauthorized")
	}
	return domain.GetBoardPermissions(ctx, boardUUID)
}

// SearchUsersForBoard finds users to invite (reuses the shared user search).
func SearchUsersForBoard(ctx context.Context, userDgraphUUID string, searchText string) (usersList []*dgraphStruct.DgraphUser, err error) {
	dgraphUsers, err := userDomain.GetUserListWithSearchText(ctx, userDgraphUUID, searchText)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SearchUsersForBoard Failed to get users list err: %+v", err)
		return nil, err
	}
	return dgraphUsers, nil
}

// GetBoardList returns the boards the user can access (owned or shared), with
// optional title search and pagination.
func GetBoardList(ctx context.Context, userUID string, search string, first int, offset int) (list *dgraphStruct.DgraphBoardList, err error) {
	if first <= 0 {
		first = 30
	}
	if offset < 0 {
		offset = 0
	}
	list, err = domain.GetDgraphBoardList(ctx, userUID, search, first, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/GetBoardList Failed to get board list err: %+v", err)
		return
	}
	return
}

// CreateBoardCommentMention persists a board-associated Comment (+ inline
// Mention node) when a board canvas comment @mentions users, then publishes
// realtime activity so the mention appears in the mentioned users' activity
// feed and the board owner is notified of the new comment. The canvas comment
// thread itself lives in Yjs; this only mirrors the mention/comment into the
// activity subsystem (like doc comments) for notifications and the feed.
//
// createdByUser is the comment author (full dgraph info).
//
// SyncBoardComment mirrors a board comment (held in Yjs) into the server-side
// stores that power activity, global search, and the permission-scoped AI
// assistant: a Postgres row, a Dgraph Comment (with comment_board edge), an
// OpenSearch comment doc, and an embedding. It is keyed by the stable Yjs
// comment id via a deterministic UUID, so repeated calls (edits / new replies)
// UPSERT the same rows instead of duplicating them. Mentioned users, when any,
// additionally get a Mention node plus a realtime activity notification.
// Authorization mirrors the canvas write gate (own / edit / comment access).
func SyncBoardComment(ctx context.Context, createdByUser *dgraphStruct.DgraphUser, createdByPostgresID uuid.UUID, boardUUID string, commentID string, commentText string, mentionedUserUUIDs []string) (err error) {
	commentText = strings.TrimSpace(commentText)
	commentID = strings.TrimSpace(commentID)
	if commentID == "" || commentText == "" {
		return errors.New("board_comment_id and comment_text are required")
	}

	// Authorize: the author must be able to edit or comment on the board (or own
	// it). This mirrors the access gate the collaboration service applies before
	// allowing canvas writes.
	board, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, createdByUser.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/SyncBoardComment Failed to get board err: %+v", err)
		return err
	}
	if board == nil {
		return errors.New("board not found")
	}
	isOwner := board.CreatedBy != nil && board.CreatedBy.Uid == createdByUser.Uid
	if !isOwner && board.HasEditAccess == 0 && board.HasCommentAccess == 0 {
		return errors.New("unauthorized: no comment access to this board")
	}

	// Resolve mentioned users in ONE batch query, skipping self, blanks and
	// duplicates. Preserve the caller's order for the activity notifications.
	seen := make(map[string]bool)
	orderedUUIDs := make([]string, 0, len(mentionedUserUUIDs))
	for _, mentionUUID := range mentionedUserUUIDs {
		if mentionUUID == "" || mentionUUID == createdByUser.Uuid || seen[mentionUUID] {
			continue
		}
		seen[mentionUUID] = true
		orderedUUIDs = append(orderedUUIDs, mentionUUID)
	}

	var mentionUserRefs []*dgraphStruct.DgraphUser
	var resolvedUsers []*dgraphStruct.DgraphUser
	if len(orderedUUIDs) > 0 {
		userMap, mErr := userDomain.GetUserDisplayMapByUUIDs(ctx, orderedUUIDs)
		if mErr != nil {
			helpers.LogErrorWithContext(ctx, "business/SyncBoardComment Failed to resolve mentioned users err: %+v", mErr)
		} else {
			for _, mentionUUID := range orderedUUIDs {
				mentionedUser, ok := userMap[mentionUUID]
				if !ok || mentionedUser == nil || mentionedUser.Uid == "" {
					continue
				}
				resolvedUsers = append(resolvedUsers, mentionedUser)
				mentionUserRefs = append(mentionUserRefs, &dgraphStruct.DgraphUser{
					DType: []string{"User"},
					Uid:   mentionedUser.Uid,
				})
			}
		}
	}

	// Stable mirror id derived from the Yjs comment id, so an edit re-syncs the
	// same rows (upsert) and a delete can target them.
	commentUUIDObj := boardCommentUUID(boardUUID, commentID)
	commentUUID := commentUUIDObj.String()
	currentTime := time.Now()
	zeroUnixTime := time.Time{}

	dgraphComment := &dgraphStruct.DgraphComment{
		DType: []string{"Comment"},
		Uid:   "uid(co)",
		Uuid:  commentUUID,
		Text:  commentText,
		Board: &dgraphStruct.DgraphBoard{
			Uid: "uid(board)",
		},
		CommentBy: &dgraphStruct.DgraphUser{
			Uid: createdByUser.Uid,
		},
		CreatedAt: &currentTime,
		DeletedAt: &zeroUnixTime,
	}
	// Attach the Mention block only when this call carries new mentions, so a
	// plain re-sync (edit / non-mention reply) never spawns duplicate Mention
	// nodes / activity entries.
	if len(mentionUserRefs) > 0 {
		dgraphComment.Mentions = &dgraphStruct.DgraphMentions{
			DType:       []string{"Mention"},
			CreatedAt:   &currentTime,
			Mentions:    mentionUserRefs,
			CommentUuid: commentUUID,
			Comment: &dgraphStruct.DgraphComment{
				Uid: "uid(co)",
			},
		}
	}
	if board.CreatedBy != nil {
		dgraphComment.ContentAddedBy = &dgraphStruct.DgraphUser{Uid: board.CreatedBy.Uid}
	}

	// Parity with doc/post/task/chat comments: idempotent Postgres `comments`
	// row so any shared comment lookup by id resolves it. Best-effort.
	if cErr := commentDomain.UpsertComment(ctx, commentUUIDObj, createdByPostgresID, currentTime); cErr != nil {
		helpers.LogErrorWithContext(ctx, "business/SyncBoardComment Failed to upsert postgres comment row err: %+v", cErr)
	}

	// Upsert the Dgraph comment (keyed by comment_uuid, so an edit updates it).
	if _, err = domain.CreateBoardCommentInDgraph(ctx, dgraphComment, boardUUID, commentUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/SyncBoardComment Failed to persist comment err: %+v", err)
		return err
	}

	// Mirror to global search + the AI embedding index with the board's
	// permissions so retrieval is scoped exactly like the board (idempotent).
	go indexAndEmbedBoardComment(context.WithoutCancel(ctx), boardUUID, commentUUID, commentText, createdByUser, currentTime)

	// Realtime activity: a MENTION for each mentioned user, and a COMMENT for the
	// board owner. Only when this call carried mentions.
	if len(resolvedUsers) > 0 {
		go publishBoardCommentActivity(createdByUser, board, commentUUID, commentText, currentTime, resolvedUsers, seen)
	}

	return nil
}

// DeleteBoardComment removes a board comment's server-side mirror when it is
// deleted in the canvas: it soft-deletes the Postgres row and Dgraph node (so
// it drops from activity) and removes the OpenSearch comment doc + embedding (so
// it stops surfacing in global search and the AI assistant). Keyed by the same
// deterministic UUID as the create/update path. Authorization mirrors the
// canvas gate. Idempotent: a no-op when nothing was ever mirrored.
func DeleteBoardComment(ctx context.Context, createdByUser *dgraphStruct.DgraphUser, boardUUID string, commentID string) (err error) {
	commentID = strings.TrimSpace(commentID)
	if commentID == "" {
		return errors.New("board_comment_id is required")
	}

	board, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, createdByUser.Uid)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteBoardComment Failed to get board err: %+v", err)
		return err
	}
	if board == nil {
		return errors.New("board not found")
	}
	isOwner := board.CreatedBy != nil && board.CreatedBy.Uid == createdByUser.Uid
	if !isOwner && board.HasEditAccess == 0 && board.HasCommentAccess == 0 {
		return errors.New("unauthorized: no comment access to this board")
	}

	commentUUIDObj := boardCommentUUID(boardUUID, commentID)
	commentUUID := commentUUIDObj.String()
	now := time.Now()

	if pErr := commentDomain.SoftDeletePostByUUIUD(ctx, commentUUIDObj, now); pErr != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteBoardComment Failed to soft-delete postgres row err: %+v", pErr)
	}
	if dErr := domain.SoftDeleteBoardCommentInDgraph(ctx, commentUUID, now); dErr != nil {
		helpers.LogErrorWithContext(ctx, "business/DeleteBoardComment Failed to soft-delete dgraph comment err: %+v", dErr)
	}

	// Remove from global search and the AI embedding index (soft-delete via the
	// shared cascading-deletion path the other comment surfaces use).
	go commentDomain.DeleteCommentWithAttachmentsInOpenSearch(
		&openSearchStruct.OpenSearchComment{Uuid: commentUUID, CommentDeletedAt: helpers.Int64Pointer(now.Unix())},
		nil,
	)
	go globalSearchDomain.SyncCascadingDeletionInOpenSearch(
		[]string{"content_uuid"},
		commentUUID,
		now.Unix(),
		[]string{"ai_embeddings"},
		"cascade",
	)
	/* AI call omitted in v1 */

	return nil
}

// indexAndEmbedBoardComment mirrors a board comment into OpenSearch (global
// search) and the AI embedding index (assistant / unified search), carrying the
// board's permission fields so retrieval is scoped exactly like the board. Runs
// in the background; every step is best-effort and independently guarded.
func indexAndEmbedBoardComment(ctx context.Context, boardUUID, commentUUID, commentText string, createdByUser *dgraphStruct.DgraphUser, currentTime time.Time) {
	perms, err := domain.GetBoardPermissions(ctx, boardUUID)
	if err != nil || perms == nil {
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/indexAndEmbedBoardComment Failed to load board permissions err: %+v", err)
		}
		return
	}

	boardPrivate := perms.IsPrivate != nil && *perms.IsPrivate
	readingUsers := boardUserUUIDs(perms.ReadingUser)
	editingUsers := boardUserUUIDs(perms.EditingUser)
	commentingUsers := boardUserUUIDs(perms.CommentingUser)
	createdByUUID := ""
	if perms.CreatedBy != nil {
		createdByUUID = perms.CreatedBy.Uuid
	}

	var byProfile *string
	if createdByUser.ProfileKey != nil {
		byProfile = createdByUser.ProfileKey
	}
	osComment := &openSearchStruct.OpenSearchComment{
		Uuid:                          commentUUID,
		CommentBody:                   commentText,
		CommentByUserUuid:             createdByUser.Uuid,
		CommentByUserFullName:         createdByUser.UserName,
		CommentByProfile:              byProfile,
		CommentBoardUuid:              boardUUID,
		CommentBoardTitle:             perms.Title,
		CommentBoardPublic:            !boardPrivate,
		CommentBoardReadingUsers:      readingUsers,
		CommentBoardEditingUsers:      editingUsers,
		CommentBoardCommentingUsers:   commentingUsers,
		CommentBoardCreatedByUserUuid: createdByUUID,
		CommentCreatedAt:              currentTime.Unix(),
		CommentDeletedAt:              nil,
	}
	if osErr := commentDomain.CreateCommentInOpenSearch(osComment); osErr != nil {
		// The doc already exists (a re-sync on edit): fall back to an update so
		// the indexed text/permissions stay current.
		commentDomain.UpdateCommentInOpenSearch(*osComment)
	}

	/* AI call omitted in v1 */
}

// boardUserUUIDs extracts the user UUIDs from a board's role-user list, skipping
// any unresolved entries.
func boardUserUUIDs(users []*dgraphStruct.DgraphUser) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u != nil && u.Uuid != "" {
			out = append(out, u.Uuid)
		}
	}
	return out
}

// publishBoardCommentActivity emits the realtime MENTION/COMMENT activity items
// for a persisted board comment. The board context (uuid + title) lets the
// client navigate to the board from the activity feed.
func publishBoardCommentActivity(createdByUser *dgraphStruct.DgraphUser, board *dgraphStruct.DgraphBoard, commentUUID string, commentText string, currentTime time.Time, mentionedUsers []*dgraphStruct.DgraphUser, mentionedSet map[string]bool) {
	boardRef := func() *dgraphStruct.DgraphBoard {
		return &dgraphStruct.DgraphBoard{
			Uuid:  board.Uuid,
			Title: board.Title,
		}
	}
	authorRef := func() *dgraphStruct.DgraphUser {
		return &dgraphStruct.DgraphUser{
			Uuid:     createdByUser.Uuid,
			UserName: createdByUser.UserName,
		}
	}

	for _, mentionedUser := range mentionedUsers {
		activityItem := &dgraphActivityModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_MENTION,
			Time:         time.Now().Format(time.RFC3339),
			Mention: &dgraphStruct.DgraphMentions{
				CommentUuid: commentUUID,
				CreatedAt:   &currentTime,
				Comment: &dgraphStruct.DgraphComment{
					Uuid:      commentUUID,
					Text:      commentText,
					CommentBy: authorRef(),
					Board:     boardRef(),
					CreatedAt: &currentTime,
				},
			},
		}
		activityBusiness.PublishActivityToUser(mentionedUser.Uuid, activityItem)
	}

	if board.CreatedBy != nil && board.CreatedBy.Uuid != "" &&
		board.CreatedBy.Uuid != createdByUser.Uuid && !mentionedSet[board.CreatedBy.Uuid] {
		activityItem := &dgraphActivityModels.UnifiedActivityItem{
			ActivityType: mqttStruct.MESSAGE_ACTIVITY_COMMENT,
			Time:         time.Now().Format(time.RFC3339),
			Comment: &dgraphStruct.DgraphComment{
				Uuid:      commentUUID,
				Text:      commentText,
				CommentBy: authorRef(),
				Board:     boardRef(),
				CreatedAt: &currentTime,
			},
		}
		activityBusiness.PublishActivityToUser(board.CreatedBy.Uuid, activityItem)
	}
}

// RecordBoardView records (deduplicated, throttled) that a user opened the
// board. Any user who can access the board (owner, granted access, or a public
// board) may be recorded; callers without access are rejected.
func RecordBoardView(ctx context.Context, boardUUID, userUID, userUUID string) error {
	board, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, userUID)
	if err != nil {
		return err
	}
	if board == nil {
		return errors.New("board not found")
	}
	if !CanRead(board, userUUID) {
		return errors.New("unauthorized")
	}
	return resourceViewBusiness.RecordView(ctx, resourceViewBusiness.ResourceBoard, boardUUID, userUUID)
}

// ListBoardViewers returns one page of the board's distinct viewers
// (most-recent first). Restricted to the board owner and editors - viewers and
// commenters cannot see who else has viewed.
func ListBoardViewers(ctx context.Context, boardUUID, userUID string, limit, offset int) (*resourceViewBusiness.ViewersPage, error) {
	board, err := domain.GetBasicDgraphBoardByUUID(ctx, boardUUID, userUID)
	if err != nil {
		return nil, err
	}
	if board == nil {
		return nil, errors.New("board not found")
	}
	isOwner := board.CreatedBy != nil && board.CreatedBy.Uid == userUID
	if !isOwner && board.HasEditAccess == 0 {
		return nil, errors.New("unauthorized")
	}
	return resourceViewBusiness.ListViewersResolved(ctx, resourceViewBusiness.ResourceBoard, boardUUID, limit, offset)
}
