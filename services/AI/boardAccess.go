package ai

// A board's entries in the AI index (W6-B1, miro-05): what the board says, and
// its comments. Both carry the board's access, so the permission filter grants
// them as the board does: its owner, the people it is shared with, or everyone
// when it is public.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BoardContentType is a board's own entry: its title and what it says.
const BoardContentType = "board"

// BoardAccess is who reaches a board: everyone when it is public, else its
// owner and the people it is shared with.
type BoardAccess struct {
	Public                       bool
	CreatedBy                    string
	Reading, Editing, Commenting []string
}

// on writes the access, and the board it is of, onto an entry.
func (a BoardAccess) on(doc *EmbeddingDoc, board string) {
	doc.BoardUUID = board
	doc.BoardPublic = a.Public
	doc.BoardCreatedByUserID = a.CreatedBy
	doc.BoardReadingUsers = a.Reading
	doc.BoardEditingUsers = a.Editing
	doc.BoardCommentingUsers = a.Commenting
}

// BoardContent is one board's entry: Text is its title and what it says.
type BoardContent struct {
	BoardUUID, Text string
	Access          BoardAccess
}

// EmbedBoardContents writes several boards' entries (StoreEmbeddings). It
// returns once they are written, so the caller knows they hold.
func EmbedBoardContents(ctx context.Context, boards []BoardContent) error {
	now := time.Now().Unix()
	docs := make([]EmbeddingDoc, len(boards))
	for i, b := range boards {
		docs[i] = EmbeddingDoc{ContentText: b.Text, ContentType: BoardContentType, ContentUUID: b.BoardUUID, CreatedDate: now}
		b.Access.on(&docs[i], b.BoardUUID)
	}
	return StoreEmbeddings(ctx, docs)
}

// boardAccessFields are the fields that name who reaches a board's entries.
var boardAccessFields = []string{"board_created_by_user_id", "board_reading_users", "board_editing_users", "board_commenting_users"}

// boardGrant is the filter's clauses that grant a board's entries to the
// user: as each uuid field, and as its keyword sub-field. An index that took
// these fields by itself mapped them as text, which splits a uuid at its
// hyphens, so a term for the whole uuid matched only the keyword form: private
// boards' comments never reached their members. board_public is set only on a
// public board's entries, so it never grants anything else.
func boardGrant(userUUID string) []string {
	who, _ := json.Marshal(userUUID)
	out := make([]string, 0, 2*len(boardAccessFields)+1)
	for _, f := range boardAccessFields {
		out = append(out, fmt.Sprintf(`{"term": {%q: %s}}`, f, who), fmt.Sprintf(`{"term": {%q: %s}}`, f+".keyword", who))
	}
	return append(out, `{"term": {"board_public": true}}`)
}

// boardClause grants the user boards' own entries, as boardGrant says.
func boardClause(userUUID string) string {
	return fmt.Sprintf(`{"bool": {"must": [{"term": {"content_type": %q}}], "should": [%s], "minimum_should_match": 1}}`,
		BoardContentType, strings.Join(boardGrant(userUUID), ","))
}

// boardAccessScript writes params.access onto an entry, and leaves one that
// says it already (the same people in any order), so the count is of the
// entries that were wrong.
const boardAccessScript = "boolean same(def a, def b) { " +
	"if (a == null) { a = []; } if (b == null) { b = []; } " +
	"if (!(a instanceof List)) { a = [a]; } if (!(b instanceof List)) { b = [b]; } " +
	"return new HashSet(a).equals(new HashSet(b)); } " +
	"def a = params.access; " +
	"if (ctx._source.board_public == a['public'] && " +
	"(ctx._source.board_created_by_user_id == null ? '' : ctx._source.board_created_by_user_id) == a['created_by'] && " +
	"same(ctx._source.board_reading_users, a['reading']) && " +
	"same(ctx._source.board_editing_users, a['editing']) && " +
	"same(ctx._source.board_commenting_users, a['commenting'])) { ctx.op = 'noop'; } " +
	"else { " +
	"ctx._source.board_public = a['public']; " +
	"ctx._source.board_created_by_user_id = a['created_by']; " +
	"ctx._source.board_reading_users = a['reading']; " +
	"ctx._source.board_editing_users = a['editing']; " +
	"ctx._source.board_commenting_users = a['commenting']; }"

// boardAccessUpdateBody is the update-by-query that writes a board's access
// onto its entries: its own, and its comments'. board_uuid is asked as the
// field and as its keyword sub-field, as boardGrant says. Pure.
func boardAccessUpdateBody(boardUUID string, a BoardAccess) ([]byte, error) {
	list := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	return json.Marshal(map[string]any{
		"script": map[string]any{
			"source": boardAccessScript,
			"lang":   "painless",
			"params": map[string]any{"access": map[string]any{
				"public": a.Public, "created_by": a.CreatedBy,
				"reading": list(a.Reading), "editing": list(a.Editing), "commenting": list(a.Commenting),
			}},
		},
		"query": map[string]any{"bool": map[string]any{
			"should": []any{
				map[string]any{"term": map[string]any{"board_uuid": boardUUID}},
				map[string]any{"term": map[string]any{"board_uuid.keyword": boardUUID}},
			},
			"minimum_should_match": 1,
		}},
	})
}

// SetBoardAccess writes a board's privacy and sharing onto its entries (its
// own and its comments'), and answers how many said something else.
func SetBoardAccess(ctx context.Context, boardUUID string, a BoardAccess) (corrected int, err error) {
	if strings.TrimSpace(boardUUID) == "" {
		return 0, nil
	}
	body, err := boardAccessUpdateBody(boardUUID, a)
	if err != nil {
		return 0, err
	}
	return updateEntries(ctx, body, "a board's access")
}
