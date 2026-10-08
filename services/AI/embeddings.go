package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

// EmbeddingDoc represents a piece of content to be embedded and stored.
// Permission fields mirror the unified search permission model.
type EmbeddingDoc struct {
	ContentText string `json:"content_text"`
	ContentType string `json:"content_type"` // "post", "chat", "doc", "task", "comment"
	ContentUUID string `json:"content_uuid"`
	CreatedDate int64  `json:"created_date,omitempty"`

	// --- Soft-deletion support (mirrors OpenSearch cascading pattern) ---
	DeletedDate int64 `json:"deleted_date,omitempty"` // 0 = active (omitted in JSON)

	// --- Foreign keys for cascading archive/delete ---
	TeamUUID string `json:"team_uuid,omitempty"`
	TaskUUID string `json:"task_uuid,omitempty"`
	DocUUID  string `json:"doc_uuid,omitempty"`
	PostUUID string `json:"post_uuid,omitempty"`
	ChatUUID string `json:"chat_uuid,omitempty"`

	// --- Post-specific permission fields ---
	ChannelUUID string `json:"channel_uuid,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`

	// --- Chat-specific permission fields ---
	ChatByUserID         string   `json:"chat_by_user_id,omitempty"`
	ChatToUserID         string   `json:"chat_to_user_id,omitempty"`
	ChatParticipantUUIDs []string `json:"chat_participant_uuids,omitempty"` // all participant UUIDs for group chats
	ChatGrpID            string   `json:"chat_grp_id,omitempty"`

	// --- Doc-specific permission fields ---
	DocPrivate         bool     `json:"doc_private"`
	DocCreatedByUserID string   `json:"doc_created_by_user_id,omitempty"`
	DocReadingUsers    []string `json:"doc_reading_users,omitempty"`
	DocEditingUsers    []string `json:"doc_editing_users,omitempty"`
	DocCommentingUsers []string `json:"doc_commenting_users,omitempty"`

	// --- Task-specific permission fields ---
	ProjectUUID       string `json:"project_uuid,omitempty"`
	TaskAssigneeUUID  string `json:"task_assignee_user_id,omitempty"`
	TaskCreatedByUUID string `json:"task_created_by_user_id,omitempty"`

	// --- Board-specific permission fields (board comments) ---
	// BoardPublic is set true ONLY for a comment on a public board, so the
	// permission filter can grant public-board comments to everyone WITHOUT a
	// blanket "false" term that would leak non-board comments (those never set
	// this field). Private-board comments rely on the membership terms below.
	BoardUUID            string   `json:"board_uuid,omitempty"`
	BoardPublic          bool     `json:"board_public,omitempty"`
	BoardCreatedByUserID string   `json:"board_created_by_user_id,omitempty"`
	BoardReadingUsers    []string `json:"board_reading_users,omitempty"`
	BoardEditingUsers    []string `json:"board_editing_users,omitempty"`
	BoardCommentingUsers []string `json:"board_commenting_users,omitempty"`

	// --- Common metadata ---
	AuthorUUID string `json:"author_uuid,omitempty"`
	AuthorName string `json:"author_name,omitempty"`
}

// embeddingDocWithVector is the stored form including the vector.
type embeddingDocWithVector struct {
	EmbeddingDoc
	Embedding []float32 `json:"embedding"`
}

// SimilarResult is a single result from a vector similarity search.
type SimilarResult struct {
	ContentText string  `json:"content_text"`
	ContentType string  `json:"content_type"`
	ContentUUID string  `json:"content_uuid"`
	ChannelUUID string  `json:"channel_uuid"`
	ChannelName string  `json:"channel_name"`
	ProjectUUID string  `json:"project_uuid"`
	AuthorName  string  `json:"author_name"`
	Score       float64 `json:"score"`

	// Chat routing fields — needed to deep-link a chat/DM highlight back to
	// the right conversation (group chat vs DM is derived from chat_grp_id).
	ChatGrpID    string `json:"chat_grp_id"`
	ChatByUserID string `json:"chat_by_user_id"`
	ChatToUserID string `json:"chat_to_user_id"`

	// Parent FKs — let a "comment" highlight deep-link to the parent it
	// belongs to (post in a channel, task, or doc). Empty for non-comments.
	PostUUID string `json:"post_uuid"`
	TaskUUID string `json:"task_uuid"`
	DocUUID  string `json:"doc_uuid"`
}

const AI_EMBEDDINGS_INDEX = "ai_embeddings"

// StoreEmbedding generates a vector embedding for the given content and stores it
// in the OpenSearch ai_embeddings index.
//
// While an embedding reindex (dimension change) is in flight, live writes
// are buffered and replayed against the rebuilt index instead of being sent
// directly — see reindex.go. This prevents new-dimension vectors from being
// rejected by the still-old-dimension index (or lost entirely) during the
// rebuild window.
func StoreEmbedding(ctx context.Context, doc EmbeddingDoc) error {
	if bufferReindexWrite(doc, false) {
		return nil
	}
	return storeEmbeddingDirect(ctx, doc)
}

// storeEmbeddingDirect performs the actual embed-and-index. It is used by
// StoreEmbedding (live path), by the reindex workers, and by the buffered
// write replay — all of which must bypass the reindex buffer.
func storeEmbeddingDirect(ctx context.Context, doc EmbeddingDoc) error {
	svc := GetService()
	if svc == nil || svc.Embedder == nil {
		return fmt.Errorf("AI embedding service not initialized")
	}

	// Skip empty content
	if strings.TrimSpace(doc.ContentText) == "" {
		return nil
	}

	// Strip embedded media BEFORE truncating and before embedding. Order matters
	// twice over: an inlined image would otherwise consume the whole input budget
	// below (so the vector would describe base64 rather than the content, making the
	// document effectively unfindable by meaning), and the payload would then be
	// stored verbatim as content_text in the ai_embeddings index.
	//
	// Same rule as every other OpenSearch write path, deliberately reusing the one
	// implementation rather than repeating it here.
	text := openSearchStruct.SanitizeIndexedText(doc.ContentText)

	// Truncate very long texts (embedding models have token limits)
	// Use rune-based slicing to avoid splitting UTF-8 characters
	textRunes := []rune(text)
	if len(textRunes) > 8000 {
		text = string(textRunes[:8000])
	}

	// Generate embedding through the metered chokepoint. Indexing is metered but
	// never refused on budget: skipping it would leave this content permanently
	// unfindable by meaning, long after the cap resets.
	vectors, err := svc.GenerateEmbeddingsForIndexing(ctx, []string{text})
	if err != nil {
		return fmt.Errorf("failed to generate embedding: %w", err)
	}
	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return fmt.Errorf("embedding returned empty vector")
	}
	vector := vectors[0]

	// Build document with vector
	stored := embeddingDocWithVector{
		EmbeddingDoc: doc,
		Embedding:    vector,
	}
	// Store the truncated text
	stored.ContentText = text

	jsonData, err := json.Marshal(stored)
	if err != nil {
		return fmt.Errorf("failed to marshal embedding doc: %w", err)
	}

	// Index into OpenSearch using content_type:content_uuid as doc ID (upsert)
	docID := fmt.Sprintf("%s:%s", doc.ContentType, doc.ContentUUID)
	_, err = opensearchInit.OpenSearchClient.Document.Create(
		ctx,
		opensearchapi.DocumentCreateReq{
			Index:      AI_EMBEDDINGS_INDEX,
			DocumentID: docID,
			// Through the shared seam, like every other OpenSearch write, so the
			// ai_embeddings index gets the same guarantee as the content indexes.
			Body: openSearchStruct.IndexReader(jsonData),
		},
	)

	if err != nil {
		// If document already exists, try update
		if strings.Contains(err.Error(), "version_conflict") {
			updateBody := fmt.Sprintf(`{"doc": %s, "doc_as_upsert": true}`, string(jsonData))
			err = opensearchInit.UpdateDocument(ctx, AI_EMBEDDINGS_INDEX, docID, openSearchStruct.IndexReader([]byte(updateBody)))
			if err != nil {
				return fmt.Errorf("failed to update embedding in OpenSearch: %w", err)
			}
		} else {
			return fmt.Errorf("failed to index embedding in OpenSearch: %w", err)
		}
	}

	return nil
}

// StoreEmbeddingAsync fires a goroutine to store the embedding — fire-and-forget.
// This matches the pattern used by existing OpenSearch indexing in the codebase.
func StoreEmbeddingAsync(doc EmbeddingDoc) {
	go func() {
		// Embeddings can occasionally hang; use a timeout context
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := StoreEmbedding(ctx, doc); err != nil {
			helpers.LogErrorWithContext(ctx,
				"AI/embeddings StoreEmbeddingAsync failed for %s:%s err: %+v",
				doc.ContentType, doc.ContentUUID, err)
		}
	}()
}

// SearchSimilar performs a k-NN vector search with per-content-type permission
// filtering that matches the unified search permission model.
//
// Permission rules (from globalSearchDomian.go):
//   - Posts:  channel_uuid ∈ channelUUIDs
//   - Chats:  chat_by_user_id = userUUID OR chat_to_user_id = userUUID
//     OR chat_participant_uuids contains userUUID
//   - Docs:   doc_private=false OR doc_created_by_user_id = userUUID
//     OR doc_reading/editing/commenting_users contains userUUID
//   - Tasks:  project_uuid ∈ projectUUIDs OR task_assignee = userUUID
//     OR task_created_by = userUUID
//
// buildPermissionFilter creates a complex bool query filter matching the unified search permission model.
func buildPermissionFilter(userUUID string, channelUUIDs []string, projectUUIDs []string, grpIDs []string) string {
	channelJSON, _ := json.Marshal(channelUUIDs)
	projectJSON, _ := json.Marshal(projectUUIDs)
	grpJSON, _ := json.Marshal(grpIDs)
	userUUIDJSON, _ := json.Marshal(userUUID)

	// Post permission: channel_uuid must be in user's accessible channels
	postClause := ""
	if len(channelUUIDs) > 0 {
		postClause = fmt.Sprintf(`{
			"bool": {
				"must": [
					{"term": {"content_type": "post"}},
					{"terms": {"channel_uuid": %s}}
				]
			}
		}`, string(channelJSON))
	}

	// Chat permission: user must be sender, recipient, or participant
	chatClause := fmt.Sprintf(`{
		"bool": {
			"must": [
				{"term": {"content_type": "chat"}}
			],
			"should": [
				{"term": {"chat_by_user_id": %s}},
				{"term": {"chat_to_user_id": %s}},
				{"term": {"chat_participant_uuids": %s}}
			],
			"minimum_should_match": 1
		}
	}`, string(userUUIDJSON), string(userUUIDJSON), string(userUUIDJSON))

	// Doc permission: public OR created by user OR user has read/edit/comment role
	docClause := fmt.Sprintf(`{
		"bool": {
			"must": [
				{"term": {"content_type": "doc"}}
			],
			"should": [
				{"term": {"doc_private": false}},
				{"term": {"doc_created_by_user_id": %s}},
				{"term": {"doc_reading_users": %s}},
				{"term": {"doc_editing_users": %s}},
				{"term": {"doc_commenting_users": %s}}
			],
			"minimum_should_match": 1
		}
	}`, string(userUUIDJSON), string(userUUIDJSON), string(userUUIDJSON), string(userUUIDJSON))

	// Task permission: project member OR assignee OR creator
	taskShouldClauses := []string{
		fmt.Sprintf(`{"term": {"task_assignee_user_id": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"task_created_by_user_id": %s}}`, string(userUUIDJSON)),
	}
	if len(projectUUIDs) > 0 {
		taskShouldClauses = append([]string{fmt.Sprintf(`{"terms": {"project_uuid": %s}}`, string(projectJSON))}, taskShouldClauses...)
	}
	taskClause := ""
	if len(taskShouldClauses) > 0 {
		taskClause = fmt.Sprintf(`{
			"bool": {
				"must": [
					{"term": {"content_type": "task"}}
				],
				"should": [%s],
				"minimum_should_match": 1
			}
		}`, strings.Join(taskShouldClauses, ","))
	}

	// Comment permission: inherits all parent entity permissions
	commentShouldClauses := []string{
		fmt.Sprintf(`{"term": {"chat_by_user_id": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"chat_to_user_id": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"chat_participant_uuids": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"doc_created_by_user_id": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"doc_reading_users": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"doc_editing_users": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"doc_commenting_users": %s}}`, string(userUUIDJSON)),
		// Public-doc grant, constrained to ACTUAL doc comments. doc_private is
		// stored false on every embedding (it has no omitempty), so a bare
		// {"doc_private": false} term would blanket-match every comment and leak
		// private channel/DM comments. Requiring doc_uuid to exist scopes this
		// grant to doc comments only (post/chat/task comments never set it).
		`{"bool": {"must": [{"exists": {"field": "doc_uuid"}}, {"term": {"doc_private": false}}]}}`,
		// Board comments inherit board permissions: owner, an explicit
		// read/edit/comment role, or a public board. board_public is set only
		// on public-board comments, so it never matches a non-board comment.
		fmt.Sprintf(`{"term": {"board_created_by_user_id": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"board_reading_users": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"board_editing_users": %s}}`, string(userUUIDJSON)),
		fmt.Sprintf(`{"term": {"board_commenting_users": %s}}`, string(userUUIDJSON)),
		`{"term": {"board_public": true}}`,
	}
	if len(channelUUIDs) > 0 {
		commentShouldClauses = append([]string{fmt.Sprintf(`{"terms": {"channel_uuid": %s}}`, string(channelJSON))}, commentShouldClauses...)
	}
	if len(projectUUIDs) > 0 {
		commentShouldClauses = append(commentShouldClauses, fmt.Sprintf(`{"terms": {"project_uuid": %s}}`, string(projectJSON)))
	}
	commentClause := ""
	if len(commentShouldClauses) > 0 {
		commentClause = fmt.Sprintf(`{
			"bool": {
				"must": [
					{"term": {"content_type": "comment"}}
				],
				"should": [%s],
				"minimum_should_match": 1
			}
		}`, strings.Join(commentShouldClauses, ","))
	}

	// Memory permission: a workspace-memory item is visible if it is scoped
	// to an accessible channel or project, OR the requesting user is its
	// owner/creator (stored in author_uuid). Mirrors the structured-store
	// permission rule in models/postgres/WorkspaceMemory.
	memoryShouldClauses := []string{
		fmt.Sprintf(`{"term": {"author_uuid": %s}}`, string(userUUIDJSON)),
	}
	if len(channelUUIDs) > 0 {
		memoryShouldClauses = append(memoryShouldClauses, fmt.Sprintf(`{"terms": {"channel_uuid": %s}}`, string(channelJSON)))
	}
	if len(projectUUIDs) > 0 {
		memoryShouldClauses = append(memoryShouldClauses, fmt.Sprintf(`{"terms": {"project_uuid": %s}}`, string(projectJSON)))
	}
	if len(grpIDs) > 0 {
		memoryShouldClauses = append(memoryShouldClauses, fmt.Sprintf(`{"terms": {"chat_grp_id": %s}}`, string(grpJSON)))
	}
	memoryClause := fmt.Sprintf(`{
		"bool": {
			"must": [
				{"term": {"content_type": "memory"}}
			],
			"should": [%s],
			"minimum_should_match": 1
		}
	}`, strings.Join(memoryShouldClauses, ","))

	// Assemble top-level should clauses
	var shouldClauses []string
	if postClause != "" {
		shouldClauses = append(shouldClauses, postClause)
	}
	if chatClause != "" {
		shouldClauses = append(shouldClauses, chatClause)
	}
	if docClause != "" {
		shouldClauses = append(shouldClauses, docClause)
	}
	if taskClause != "" {
		shouldClauses = append(shouldClauses, taskClause)
	}
	if commentClause != "" {
		shouldClauses = append(shouldClauses, commentClause)
	}
	if memoryClause != "" {
		shouldClauses = append(shouldClauses, memoryClause)
	}

	return fmt.Sprintf(`{
		"bool": {
			"should": [%s],
			"minimum_should_match": 1,
			"must_not": [{"exists": {"field": "deleted_date"}}]
		}
	}`, strings.Join(shouldClauses, ","))
}

// SearchSimilar performs a vector search in OpenSearch with permission filtering.
func SearchSimilar(ctx context.Context, query string, userUUID string, channelUUIDs []string, projectUUIDs []string, grpIDs []string, limit int) ([]SimilarResult, error) {
	svc := GetService()
	if svc == nil || svc.Embedder == nil {
		return nil, fmt.Errorf("AI embedding service not initialized")
	}

	if limit <= 0 {
		limit = 5
	}

	// Embed the query through the metered chokepoint. On-demand work, so this IS
	// refused once a daily cap is exhausted (search_workspace over MCP included).
	queryVectors, err := svc.GenerateEmbeddings(ctx, []string{query})
	if err != nil {
		return nil, fmt.Errorf("failed to embed query: %w", err)
	}
	if len(queryVectors) == 0 || len(queryVectors[0]) == 0 {
		return nil, fmt.Errorf("query embedding returned empty vector")
	}
	queryVector := queryVectors[0]
	vectorJSON, _ := json.Marshal(queryVector)

	permissionFilter := buildPermissionFilter(userUUID, channelUUIDs, projectUUIDs, grpIDs)

	searchBody := fmt.Sprintf(`{
		"size": %d,
		"query": {
			"knn": {
				"embedding": {
					"vector": %s,
					"k": %d,
					"filter": %s
				}
			}
		},
		"_source": ["content_text", "content_type", "content_uuid", "channel_uuid", "channel_name", "project_uuid", "author_name"]
	}`, limit, string(vectorJSON), limit, permissionFilter)

	// Execute search
	type knnHit struct {
		Score  float64         `json:"_score"`
		Source json.RawMessage `json:"_source"`
	}
	type knnResp struct {
		Hits struct {
			Hits []knnHit `json:"hits"`
		} `json:"hits"`
	}

	var resp knnResp
	_, err = opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(searchBody),
	}, &resp)

	if err != nil {
		return nil, fmt.Errorf("k-NN search failed: %w", err)
	}

	// Parse results
	results := make([]SimilarResult, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var result SimilarResult
		if err := json.Unmarshal(hit.Source, &result); err != nil {
			helpers.LogErrorWithContext(ctx,
				"AI/embeddings SearchSimilar failed to unmarshal hit: %+v", err)
			continue
		}
		result.Score = hit.Score
		results = append(results, result)
	}

	return KeepLive(ctx, results), nil
}

// SearchRecent fetches the most recent content from OpenSearch based on a filter
// (e.g. channel_uuid or chat_grp_id) and sorts by created_date.
// It enforces the same permission model as SearchSimilar.
func SearchRecent(ctx context.Context, userUUID string, channelUUIDs []string, projectUUIDs []string, grpIDs []string, filterType string, filterValue string, limit int) ([]SimilarResult, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("OpenSearch client not initialized")
	}

	if limit <= 0 {
		limit = 15
	}

	// Build filter based on type
	var termClause string
	switch filterType {
	case "channel_uuid":
		termClause = fmt.Sprintf(`{"term": {"channel_uuid": "%s"}}`, filterValue)
	case "grp_id", "chat_grp_id":
		termClause = fmt.Sprintf(`{"term": {"chat_grp_id": "%s"}}`, filterValue)
	default:
		return nil, fmt.Errorf("unsupported filter type: %s", filterType)
	}

	// Build unified permission filter
	permissionFilter := buildPermissionFilter(userUUID, channelUUIDs, projectUUIDs, grpIDs)

	searchBody := fmt.Sprintf(`{
		"size": %d,
		"query": {
			"bool": {
				"must": [
					%s,
					%s
				],
				"must_not": [{"exists": {"field": "deleted_date"}}]
			}
		},
		"sort": [
			{"created_date": {"order": "desc"}}
		],
		"_source": ["content_text", "content_type", "content_uuid", "channel_uuid", "channel_name", "project_uuid", "author_name"]
	}`, limit, termClause, permissionFilter)

	type osHit struct {
		Source json.RawMessage `json:"_source"`
	}
	type osResp struct {
		Hits struct {
			Hits []osHit `json:"hits"`
		} `json:"hits"`
	}

	var resp osResp
	_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(searchBody),
	}, &resp)

	if err != nil {
		return nil, fmt.Errorf("chronological search failed: %w", err)
	}

	results := make([]SimilarResult, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var result SimilarResult
		if err := json.Unmarshal(hit.Source, &result); err != nil {
			continue
		}
		results = append(results, result)
	}

	return KeepLive(ctx, results), nil
}

// SearchRecentGlobal fetches the most recent content across ALL accessible resources. Used when the user asks for broad updates (e.g. "what are the recent updates?")
// without specifying a specific channel or DM.
// Applies the same permission model as SearchSimilar.
func SearchRecentGlobal(ctx context.Context, userUUID string, channelUUIDs []string, projectUUIDs []string, grpIDs []string, limit int) ([]SimilarResult, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("OpenSearch client not initialized")
	}

	if limit <= 0 {
		limit = 15
	}

	// Build unified permission filter (same as SearchSimilar)
	permissionFilter := buildPermissionFilter(userUUID, channelUUIDs, projectUUIDs, grpIDs)

	// Highlights = recent raw workspace ACTIVITY (posts/chats/docs/tasks/
	// comments). We exclude content_type:memory here for two reasons:
	//   1. The user's own structured memory (open commitments/questions) has
	//      its own dedicated "open items" column in the briefing, so showing
	//      memory facts in highlights is redundant.
	//   2. Memory embeddings are NOT removed when an item is resolved/dismissed
	//      (only on hard delete), so a resolved commitment would otherwise keep
	//      surfacing here. Excluding the type keeps highlights correct
	//      regardless of memory-item lifecycle.
	searchBody := fmt.Sprintf(`{
		"size": %d,
		"query": {
			"bool": {
				"must": [
					%s
				],
				"must_not": [
					{"exists": {"field": "deleted_date"}},
					{"term": {"content_type": "memory"}}
				]
			}
		},
		"sort": [
			{"created_date": {"order": "desc"}}
		],
		"_source": ["content_text", "content_type", "content_uuid", "channel_uuid", "channel_name", "project_uuid", "author_name", "chat_grp_id", "chat_by_user_id", "chat_to_user_id", "post_uuid", "task_uuid", "doc_uuid"]
	}`, limit, permissionFilter)

	type osHit struct {
		Source json.RawMessage `json:"_source"`
	}
	type osResp struct {
		Hits struct {
			Hits []osHit `json:"hits"`
		} `json:"hits"`
	}

	var resp osResp
	_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(searchBody),
	}, &resp)

	if err != nil {
		return nil, fmt.Errorf("global chronological search failed: %w", err)
	}

	results := make([]SimilarResult, 0, len(resp.Hits.Hits))
	for _, hit := range resp.Hits.Hits {
		var result SimilarResult
		if err := json.Unmarshal(hit.Source, &result); err != nil {
			continue
		}
		results = append(results, result)
	}

	return KeepLive(ctx, results), nil
}

// DeleteEmbedding removes an embedding from the index (e.g. when content is deleted).
//
// During a reindex it is buffered (as a tombstone) and replayed against the
// rebuilt index, so a deletion that happens mid-rebuild isn't resurrected by
// the snapshot-then-recreate flow.
func DeleteEmbedding(ctx context.Context, contentType string, contentUUID string) error {
	if bufferReindexWrite(EmbeddingDoc{ContentType: contentType, ContentUUID: contentUUID}, true) {
		return nil
	}
	return deleteEmbeddingDirect(ctx, contentType, contentUUID)
}

// deleteEmbeddingDirect performs the actual index delete, bypassing the
// reindex buffer (used by the live path and the buffered-write replay).
func deleteEmbeddingDirect(ctx context.Context, contentType string, contentUUID string) error {
	docID := fmt.Sprintf("%s:%s", contentType, contentUUID)
	if err := opensearchInit.DeleteDocument(ctx, AI_EMBEDDINGS_INDEX, docID); err != nil {
		return fmt.Errorf("failed to delete embedding: %w", err)
	}
	return nil
}

// MigrateEmbeddingsGroupID updates all AI embeddings that reference oldGrpID:
// 1. Sets chat_grp_id to newGrpID
// 2. Appends newParticipantUUID to chat_participant_uuids
// This must be called when AddParticipantToGroupChat changes the group ID,
// otherwise AI search/summarization breaks for the group.
func MigrateEmbeddingsGroupID(ctx context.Context, oldGrpID string, newGrpID string, newParticipantUUID string) error {
	if opensearchInit.OpenSearchClient == nil {
		return fmt.Errorf("OpenSearch client not initialized")
	}

	var boolTrue = true

	// Painless script: update chat_grp_id and append participant UUID if not already present
	body := strings.NewReader(fmt.Sprintf(`{
		"script": {
			"source": "ctx._source.chat_grp_id = params.newGrpId; if (ctx._source.chat_participant_uuids == null) { ctx._source.chat_participant_uuids = [params.newParticipantUUID]; } else if (!ctx._source.chat_participant_uuids.contains(params.newParticipantUUID)) { ctx._source.chat_participant_uuids.add(params.newParticipantUUID); }",
			"lang": "painless",
			"params": {
				"newGrpId": "%s",
				"newParticipantUUID": "%s"
			}
		},
		"query": {
			"term": {
				"chat_grp_id": "%s"
			}
		}
	}`, newGrpID, newParticipantUUID, oldGrpID))

	resp, err := opensearchInit.OpenSearchClient.UpdateByQuery(ctx, opensearchapi.UpdateByQueryReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    body,
		Params: opensearchapi.UpdateByQueryParams{
			Refresh:           &boolTrue,
			WaitForCompletion: &boolTrue,
			Conflicts:         "proceed",
		},
	})

	if err != nil {
		return fmt.Errorf("AI embeddings group migration failed: %w", err)
	}

	if len(resp.Failures) > 0 {
		return fmt.Errorf("AI embeddings group migration had %d failures", len(resp.Failures))
	}

	return nil
}

// MigrateEmbeddingsGroupIDAsync fires a goroutine to migrate AI embeddings — fire-and-forget.
func MigrateEmbeddingsGroupIDAsync(oldGrpID string, newGrpID string, newParticipantUUID string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		if err := MigrateEmbeddingsGroupID(ctx, oldGrpID, newGrpID, newParticipantUUID); err != nil {
			helpers.LogErrorWithContext(ctx,
				"AI/embeddings MigrateEmbeddingsGroupIDAsync failed for %s -> %s err: %+v",
				oldGrpID, newGrpID, err)
		}
	}()
}

// --- Convenience wrappers ---

// EmbedPostContent is a convenience wrapper for embedding a channel post.
func EmbedPostContent(postText string, postUUID string, channelUUID string, channelName string, authorUUID string, authorName string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText: postText,
		ContentType: "post",
		ContentUUID: postUUID,
		ChannelUUID: channelUUID,
		ChannelName: channelName,
		AuthorUUID:  authorUUID,
		AuthorName:  authorName,
		CreatedDate: time.Now().Unix(),
	})
}

// EmbedChatContent is a convenience wrapper for embedding a chat message.
// Includes participant UUIDs for permission-aware search.
func EmbedChatContent(chatText string, chatUUID string, authorUUID string, authorName string, chatGrpID string, chatByUserID string, chatToUserID string, participantUUIDs []string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText:          chatText,
		ContentType:          "chat",
		ContentUUID:          chatUUID,
		ChatUUID:             chatUUID,
		ChatByUserID:         chatByUserID,
		ChatToUserID:         chatToUserID,
		ChatParticipantUUIDs: participantUUIDs,
		ChatGrpID:            chatGrpID,
		AuthorUUID:           authorUUID,
		AuthorName:           authorName,
		CreatedDate:          time.Now().Unix(),
	})
}

// EmbedDocContent is a convenience wrapper for embedding a document.
// Includes doc permission fields for permission-aware search.
func EmbedDocContent(docTitle string, docBody string, docUUID string, authorUUID string, authorName string, docPrivate bool, docCreatedByUserID string, readingUsers []string, editingUsers []string, commentingUsers []string) {
	// Doc bodies are rich-text/HTML from the editor (e.g. <p class="text-node">).
	// Strip to plain text before embedding so (a) previews/snippets never show
	// raw tags and (b) the embedding reflects real prose, not markup. Posts,
	// chats, and tasks already pass plain text; this brings docs in line.
	body := helpers.HTMLToPlainText(docBody)
	text := docTitle
	if body != "" {
		text = docTitle + "\n\n" + body
	}
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText:        text,
		ContentType:        "doc",
		ContentUUID:        docUUID,
		DocPrivate:         docPrivate,
		DocCreatedByUserID: docCreatedByUserID,
		DocReadingUsers:    readingUsers,
		DocEditingUsers:    editingUsers,
		DocCommentingUsers: commentingUsers,
		AuthorUUID:         authorUUID,
		AuthorName:         authorName,
		CreatedDate:        time.Now().Unix(),
	})
}

// EmbedTaskContent is a convenience wrapper for embedding a task.
func EmbedTaskContent(taskName string, taskDesc string, taskUUID string, projectUUID string, teamUUID string, assigneeUUID string, createdByUUID string, createdByName string) {
	text := taskName
	if taskDesc != "" {
		text = taskName + "\n\n" + taskDesc
	}
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText:       text,
		ContentType:       "task",
		ContentUUID:       taskUUID,
		TaskUUID:          taskUUID,
		ProjectUUID:       projectUUID,
		TeamUUID:          teamUUID,
		TaskAssigneeUUID:  assigneeUUID,
		TaskCreatedByUUID: createdByUUID,
		AuthorUUID:        createdByUUID,
		AuthorName:        createdByName,
		CreatedDate:       time.Now().Unix(),
	})
}

// EmbedPostCommentContent embeds a comment on a post, inheriting channel permissions.
func EmbedPostCommentContent(commentText string, commentUUID string, authorUUID string, authorName string,
	channelUUID string, postUUID string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText: commentText,
		ContentType: "comment",
		ContentUUID: commentUUID,
		PostUUID:    postUUID,
		ChannelUUID: channelUUID,
		AuthorUUID:  authorUUID,
		AuthorName:  authorName,
		CreatedDate: time.Now().Unix(),
	})
}

// EmbedTaskCommentContent embeds a comment on a task, inheriting project/team permissions.
func EmbedTaskCommentContent(commentText string, commentUUID string, authorUUID string, authorName string,
	projectUUID string, taskUUID string, teamUUID string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText: commentText,
		ContentType: "comment",
		ContentUUID: commentUUID,
		TaskUUID:    taskUUID,
		ProjectUUID: projectUUID,
		TeamUUID:    teamUUID,
		AuthorUUID:  authorUUID,
		AuthorName:  authorName,
		CreatedDate: time.Now().Unix(),
	})
}

// EmbedDocCommentContent embeds a comment on a doc, inheriting doc permissions.
func EmbedDocCommentContent(commentText string, commentUUID string, authorUUID string, authorName string,
	docUUID string, docPrivate bool, docCreatedByUserID string,
	docReadingUsers []string, docEditingUsers []string, docCommentingUsers []string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText:        commentText,
		ContentType:        "comment",
		ContentUUID:        commentUUID,
		DocUUID:            docUUID,
		DocPrivate:         docPrivate,
		DocCreatedByUserID: docCreatedByUserID,
		DocReadingUsers:    docReadingUsers,
		DocEditingUsers:    docEditingUsers,
		DocCommentingUsers: docCommentingUsers,
		AuthorUUID:         authorUUID,
		AuthorName:         authorName,
		CreatedDate:        time.Now().Unix(),
	})
}

// EmbedChatCommentContent embeds a comment on a chat, inheriting participant permissions.
func EmbedChatCommentContent(commentText string, commentUUID string, authorUUID string, authorName string,
	chatUUID string, chatGrpID string, chatByUserID string, chatToUserID string, chatParticipantUUIDs []string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText:          commentText,
		ContentType:          "comment",
		ContentUUID:          commentUUID,
		ChatUUID:             chatUUID,
		ChatGrpID:            chatGrpID,
		ChatByUserID:         chatByUserID,
		ChatToUserID:         chatToUserID,
		ChatParticipantUUIDs: chatParticipantUUIDs,
		AuthorUUID:           authorUUID,
		AuthorName:           authorName,
		CreatedDate:          time.Now().Unix(),
	})
}

// EmbedBoardCommentContent embeds a comment on a board, inheriting board
// permissions (owner, read/edit/comment roles, or a public board). boardPrivate
// is translated into BoardPublic so the permission filter can grant public
// boards without a blanket term that would leak other comment types.
func EmbedBoardCommentContent(commentText string, commentUUID string, authorUUID string, authorName string,
	boardUUID string, boardPrivate bool, boardCreatedByUserID string,
	boardReadingUsers []string, boardEditingUsers []string, boardCommentingUsers []string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText:          commentText,
		ContentType:          "comment",
		ContentUUID:          commentUUID,
		BoardUUID:            boardUUID,
		BoardPublic:          !boardPrivate,
		BoardCreatedByUserID: boardCreatedByUserID,
		BoardReadingUsers:    boardReadingUsers,
		BoardEditingUsers:    boardEditingUsers,
		BoardCommentingUsers: boardCommentingUsers,
		AuthorUUID:           authorUUID,
		AuthorName:           authorName,
		CreatedDate:          time.Now().Unix(),
	})
}

// EmbedMemoryContent embeds a workspace-memory item so semantic search
// (RAG) surfaces structured facts (decisions/commitments/questions)
// alongside raw content. Scope fields drive permission filtering exactly
// like posts/tasks. authorUUID is the owner/creator, used as the
// owner-visibility key in buildPermissionFilter. Channel/project may be
// empty when the item is scoped only to a DM/group (chat_grp_id).
func EmbedMemoryContent(text, memoryUUID, channelUUID, projectUUID, chatGrpID, authorUUID string) {
	StoreEmbeddingAsync(EmbeddingDoc{
		ContentText: text,
		ContentType: "memory",
		ContentUUID: memoryUUID,
		ChannelUUID: channelUUID,
		ProjectUUID: projectUUID,
		ChatGrpID:   chatGrpID,
		AuthorUUID:  authorUUID,
		CreatedDate: time.Now().Unix(),
	})
}

// ScopedContent is a lightweight content row used by the batched memory
// extractor: text + author, for a single scope (channel or group) within
// a time window.
type ScopedContent struct {
	AuthorName  string
	ContentText string
	CreatedDate int64
}

// memoryScopeFields enumerates the embedding-doc fields that can anchor a
// batched-extraction scope. A keyword field per scope dimension; comments
// inherit their parent entity's field, so a single terms filter sweeps
// posts+comments (channel), chats+comments (group), tasks+comments
// (project) without per-type branching.
var memoryScopeFields = map[string]bool{
	"channel_uuid": true,
	"chat_grp_id":  true,
	"project_uuid": true,
}

// FetchContentSince returns up to `limit` conversational content items
// (posts/chats/tasks/comments — NEVER memory items) for a scope, created
// strictly AFTER `sinceUnix`, in chronological order. filterType is one of
// channel_uuid / chat_grp_id / project_uuid. Used by the batched memory
// extractor to build a per-scope window without a separate content store —
// the embeddings index already holds the text + author + timestamp.
func FetchContentSince(ctx context.Context, filterType, filterValue string, sinceUnix int64, limit int) ([]ScopedContent, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("OpenSearch client not initialized")
	}
	if limit <= 0 || limit > 1000 {
		limit = 300
	}
	if !memoryScopeFields[filterType] {
		return nil, fmt.Errorf("unsupported filter type: %s", filterType)
	}
	termClause := fmt.Sprintf(`{"term": {"%s": "%s"}}`, filterType, filterValue)

	// Scope field + time window define the set; we exclude memory items
	// (so extraction never feeds on its own output) and soft-deleted
	// content (so deleted text can't be re-extracted). gt:sinceUnix makes
	// the window incremental against the per-scope watermark.
	searchBody := fmt.Sprintf(`{
		"size": %d,
		"query": {
			"bool": {
				"must": [
					%s,
					{"range": {"created_date": {"gt": %d}}}
				],
				"must_not": [
					{"exists": {"field": "deleted_date"}},
					{"term": {"content_type": "memory"}}
				]
			}
		},
		"sort": [{"created_date": {"order": "asc"}}],
		"_source": ["content_text", "author_name", "created_date"]
	}`, limit, termClause, sinceUnix)

	type osHit struct {
		Source json.RawMessage `json:"_source"`
	}
	type osResp struct {
		Hits struct {
			Hits []osHit `json:"hits"`
		} `json:"hits"`
	}

	var resp osResp
	_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(searchBody),
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("fetch content since failed: %w", err)
	}

	out := make([]ScopedContent, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		var row struct {
			ContentText string `json:"content_text"`
			AuthorName  string `json:"author_name"`
			CreatedDate int64  `json:"created_date"`
		}
		if err := json.Unmarshal(h.Source, &row); err != nil {
			continue
		}
		if strings.TrimSpace(row.ContentText) == "" {
			continue
		}
		out = append(out, ScopedContent{
			AuthorName:  row.AuthorName,
			ContentText: row.ContentText,
			CreatedDate: row.CreatedDate,
		})
	}
	return out, nil
}

// FetchUnreadAcrossScopes returns conversational content (posts/chats/tasks/
// comments — never memory items) created strictly AFTER sinceUnix across ALL
// of the caller's accessible scopes, in chronological order. It is the
// workspace-wide "catch me up on everything" window: same permission model
// as SearchSimilar/SearchRecent (buildPermissionFilter), excludes memory and
// soft-deleted content, bounded by limit.
//
// To keep a coherent recap of the MOST RECENT activity on a very busy
// workspace, it sorts DESC for the size cap (newest N), then the caller/
// formatter reads them oldest-first — so we return ascending here after a
// bounded fetch.
func FetchUnreadAcrossScopes(ctx context.Context, userUUID string, channelUUIDs, projectUUIDs, grpIDs []string, sinceUnix int64, limit int) ([]ScopedContent, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("OpenSearch client not initialized")
	}
	if limit <= 0 || limit > 1000 {
		limit = 300
	}

	permissionFilter := buildPermissionFilter(userUUID, channelUUIDs, projectUUIDs, grpIDs)

	// Newest-first for the size cap so a flood of old content can't bury the
	// most recent unread; we re-sort to chronological after fetching.
	searchBody := fmt.Sprintf(`{
		"size": %d,
		"query": {
			"bool": {
				"must": [
					%s,
					{"range": {"created_date": {"gt": %d}}}
				],
				"must_not": [
					{"exists": {"field": "deleted_date"}},
					{"term": {"content_type": "memory"}}
				]
			}
		},
		"sort": [{"created_date": {"order": "desc"}}],
		"_source": ["content_text", "author_name", "created_date"]
	}`, limit, permissionFilter, sinceUnix)

	type osHit struct {
		Source json.RawMessage `json:"_source"`
	}
	type osResp struct {
		Hits struct {
			Hits []osHit `json:"hits"`
		} `json:"hits"`
	}

	var resp osResp
	_, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(searchBody),
	}, &resp)
	if err != nil {
		return nil, fmt.Errorf("fetch unread across scopes failed: %w", err)
	}

	out := make([]ScopedContent, 0, len(resp.Hits.Hits))
	for _, h := range resp.Hits.Hits {
		var row struct {
			ContentText string `json:"content_text"`
			AuthorName  string `json:"author_name"`
			CreatedDate int64  `json:"created_date"`
		}
		if err := json.Unmarshal(h.Source, &row); err != nil {
			continue
		}
		if strings.TrimSpace(row.ContentText) == "" {
			continue
		}
		out = append(out, ScopedContent{
			AuthorName:  row.AuthorName,
			ContentText: row.ContentText,
			CreatedDate: row.CreatedDate,
		})
	}
	// Re-sort ascending (chronological) for a natural recap reading order.
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedDate < out[j].CreatedDate })
	return out, nil
}

type ActiveScope struct {
	FilterType  string // "channel_uuid" | "chat_grp_id" | "project_uuid"
	FilterValue string
	DocCount    int64
}

// DiscoverActiveScopes returns the distinct scopes that have received new
// conversational content (posts/chats/comments — never memory) since
// `sinceUnix`, via a single terms aggregation per scope dimension. This is
// how the batched memory extractor finds what to work on WITHOUT
// enumerating every channel/DM in the graph: it only visits scopes that
// actually changed, which keeps each pass O(active scopes) rather than
// O(all scopes). `maxPerDimension` bounds the aggregation cardinality so a
// huge workspace can't produce an unbounded work list in one tick.
func DiscoverActiveScopes(ctx context.Context, sinceUnix int64, maxPerDimension int) ([]ActiveScope, error) {
	if opensearchInit.OpenSearchClient == nil {
		return nil, fmt.Errorf("OpenSearch client not initialized")
	}
	if maxPerDimension <= 0 || maxPerDimension > 5000 {
		maxPerDimension = 1000
	}

	// One aggregation per scope dimension. min_doc_count:1 + the range
	// filter means only scopes with NEW content appear. We aggregate over
	// post/chat/comment only; memory + soft-deleted are excluded by the
	// query, so extraction never feeds on its own output or on deleted text.
	body := fmt.Sprintf(`{
		"size": 0,
		"query": {
			"bool": {
				"must": [
					{"range": {"created_date": {"gt": %d}}}
				],
				"must_not": [
					{"exists": {"field": "deleted_date"}},
					{"term": {"content_type": "memory"}}
				]
			}
		},
		"aggs": {
			"channels": {"terms": {"field": "channel_uuid", "size": %d, "min_doc_count": 1}},
			"groups":   {"terms": {"field": "chat_grp_id", "size": %d, "min_doc_count": 1}},
			"projects": {"terms": {"field": "project_uuid", "size": %d, "min_doc_count": 1}}
		}
	}`, sinceUnix, maxPerDimension, maxPerDimension, maxPerDimension)

	type bucket struct {
		Key      string `json:"key"`
		DocCount int64  `json:"doc_count"`
	}
	type termsAgg struct {
		Buckets []bucket `json:"buckets"`
	}
	var resp struct {
		Aggregations struct {
			Channels termsAgg `json:"channels"`
			Groups   termsAgg `json:"groups"`
			Projects termsAgg `json:"projects"`
		} `json:"aggregations"`
	}

	if _, err := opensearchInit.OpenSearchClient.Client.Do(ctx, opensearchapi.SearchReq{
		Indices: []string{AI_EMBEDDINGS_INDEX},
		Body:    strings.NewReader(body),
	}, &resp); err != nil {
		return nil, fmt.Errorf("discover active scopes failed: %w", err)
	}

	out := make([]ActiveScope, 0,
		len(resp.Aggregations.Channels.Buckets)+
			len(resp.Aggregations.Groups.Buckets)+
			len(resp.Aggregations.Projects.Buckets))
	appendBuckets := func(filterType string, buckets []bucket) {
		for _, b := range buckets {
			if strings.TrimSpace(b.Key) == "" {
				continue
			}
			out = append(out, ActiveScope{
				FilterType:  filterType,
				FilterValue: b.Key,
				DocCount:    b.DocCount,
			})
		}
	}
	appendBuckets("channel_uuid", resp.Aggregations.Channels.Buckets)
	appendBuckets("chat_grp_id", resp.Aggregations.Groups.Buckets)
	appendBuckets("project_uuid", resp.Aggregations.Projects.Buckets)
	return out, nil
}
