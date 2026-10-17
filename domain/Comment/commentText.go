package domain

import (
	"context"

	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Comment"
)

// GetCommentForTelling is what telling people about a comment reads of it: its
// words as stored, and who it mentions (user uuids). It decides nothing about
// who may read them: the caller has.
func GetCommentForTelling(ctx context.Context, commentUUID string) (text string, mentioned []string, err error) {
	c, err := dgraphModels.GetDgraphCommentInfoByUUID(ctx, `query CommentInfo($id: string){
				commentInfo(func: eq(comment_uuid, $id)) {
					uid
					comment_text
					comment_mentions {
						mention_users { user_uuid }
					}
				}
			}`, map[string]string{"$id": commentUUID})
	if err != nil || c == nil {
		return "", nil, err
	}
	if c.Mentions != nil {
		for _, u := range c.Mentions.Mentions {
			if u != nil && u.Uuid != "" {
				mentioned = append(mentioned, u.Uuid)
			}
		}
	}
	return c.Text, mentioned, nil
}
