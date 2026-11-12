package domain

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
)

// mentionOf is the predicate naming the thing a mention node is of, by kind.
var mentionOf = map[string]string{
	"task":    "mention_task_uuid",
	"post":    "mention_post_uuid",
	"chat":    "mention_chat_uuid",
	"comment": "mention_comment_uuid",
}

// Mentioned is who the mention node of a thing names now (user uuids): kind
// is what holds it ("task", "post", "chat" or "comment"), id its uuid. An
// edit reads it before its write, which replaces who it names, so only the
// people the edit adds are told (business/Notification TellMentionsAdded).
// One indexed read; none for a kind it doesn't know.
func Mentioned(ctx context.Context, kind, id string) ([]string, error) {
	pred, ok := mentionOf[kind]
	if !ok || id == "" {
		return nil, nil
	}
	var out struct {
		M []struct {
			Users []struct {
				UUID string `json:"user_uuid"`
			} `json:"mention_users"`
		} `json:"m"`
	}
	q := `query Mentioned($id: string){ m(func: eq(` + pred + `, $id)) { mention_users { user_uuid } } }`
	if err := dgraphInit.ReadInto(ctx, q, map[string]string{"$id": id}, &out); err != nil {
		helpers.LogErrorWithContext(ctx, "domain/Activity Mentioned %s %s err: %+v", kind, id, err)
		return nil, err
	}
	var ids []string
	for _, m := range out.M {
		for _, u := range m.Users {
			if u.UUID != "" {
				ids = append(ids, u.UUID)
			}
		}
	}
	return ids, nil
}
