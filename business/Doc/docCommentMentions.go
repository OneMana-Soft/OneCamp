package businness

import (
	"context"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// tellDocCommentMentionsAdded tells, in the background, whom an edit of a
// doc's comment newly names (s, read before the edit's write): each who may
// read the doc now (Readers), as a comment's mention reaches them, told in the
// inbox, by push and by email (doc.mentioned, with the comment's id).
func tellDocCommentMentionsAdded(ctx context.Context, actor *dgraphStruct.DgraphUser, doc *dgraphStruct.DgraphDoc, commentUUID, html string, s notificationBusiness.MentionSave) {
	if actor == nil || doc == nil || doc.Uuid == "" {
		return
	}
	s.Hear = func(ctx context.Context, added []string) ([]string, error) {
		may, err := Readers(ctx, doc.Uuid, added)
		if err != nil {
			return nil, err
		}
		var hear []string
		for _, id := range added {
			if may[id] {
				hear = append(hear, id)
			}
		}
		return hear, nil
	}
	where := doc.Title
	if where == "" {
		where = "a doc"
	}
	n := notificationBusiness.Notice{
		Kind:          string(resourceEventBusiness.DocMentioned),
		ResourceKind:  "doc",
		ResourceUUID:  doc.Uuid,
		ResourceTitle: doc.Title,
		Actor:         actor,
		Data:          map[string]string{"comment_uuid": commentUUID},
		Title:         actor.DisplayName() + " mentioned you in a comment on " + where,
		Body:          helpers.HTMLToPlainText(html),
		Path:          "/app/doc/" + doc.Uuid,
		Email:         notificationBusiness.EventDocMention,
	}
	ctx = context.WithoutCancel(ctx)
	helpers.GoSafeNamed("business/Doc tellDocCommentMentionsAdded", func() {
		notificationBusiness.TellMentionsAdded(ctx, n, s)
	})
}
