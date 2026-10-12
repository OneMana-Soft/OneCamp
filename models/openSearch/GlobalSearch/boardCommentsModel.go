package models

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/initializers/opensearchInit"
)

// BoardCommentAccess is what a board comment's search entry carries of its
// board's privacy and sharing (business/Board indexAndEmbedBoardComment).
type BoardCommentAccess struct {
	Public     bool     `json:"public"`
	Reading    []string `json:"reading"`
	Editing    []string `json:"editing"`
	Commenting []string `json:"commenting"`
	CreatedBy  string   `json:"createdBy"`
}

// boardCommentsScript writes a board's privacy and sharing onto one of its
// comments' entries.
const boardCommentsScript = `
ctx._source.comment_board_public = params.access.public;
ctx._source.comment_board_reading_users = params.access.reading;
ctx._source.comment_board_editing_users = params.access.editing;
ctx._source.comment_board_commenting_users = params.access.commenting;
ctx._source.comment_board_created_by_user_id = params.access.createdBy;
`

// SetBoardCommentAccess writes a board's privacy and sharing onto every one of
// its comments' search entries, in one update-by-query, and answers how many
// it changed. The comments index maps no board field of its own, so the
// board's id is matched as itself and as its keyword sub-field (exactClauses).
func SetBoardCommentAccess(ctx context.Context, boardUUID string, access BoardCommentAccess) (int, error) {
	if boardUUID == "" {
		return 0, nil
	}
	if opensearchInit.OpenSearchClient == nil {
		return 0, fmt.Errorf("search is not connected")
	}
	for _, list := range []*[]string{&access.Reading, &access.Editing, &access.Commenting} {
		if *list == nil {
			*list = []string{}
		}
	}
	params, _ := json.Marshal(map[string]any{"access": access})
	scriptJSON, _ := json.Marshal(boardCommentsScript)
	body := fmt.Sprintf(`{
		"script": {"source": %s, "lang": "painless", "params": %s},
		"query": {"bool": {"should": [%s], "minimum_should_match": 1}}
	}`, scriptJSON, params, strings.Join(exactClauses("comment_board_id", []string{boardUUID}), ","))
	updated, err := updateByQuerySettled(ctx, []string{"comments"}, body)
	if err != nil {
		return updated, fmt.Errorf("board comment entries: %w", err)
	}
	return updated, nil
}
