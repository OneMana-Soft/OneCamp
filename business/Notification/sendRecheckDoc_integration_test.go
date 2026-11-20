//go:build integration

package notification_test

// A doc's comment waits in the queue as any email may, and the doc changes
// meanwhile. At the send the worker asks again what the live path asked: the
// doc is still there and the person may still read it (business/Doc's read
// rule, which it hands the worker when the server starts: so this test lives
// outside the package and imports business/Doc), and the comment is still
// there, a member's or a guest's. Docs keep no notification level and no read
// marker for comments, so neither is asked.
//
// Run: go test -tags=integration ./business/Notification/ -run TestADocCommentStillHoldsWhenItIsSent -v

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/business/Access/level"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	notification "github.com/akashc777/OneCamp/business/Notification"
	notifylevel "github.com/akashc777/OneCamp/business/NotifyLevel"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestADocCommentStillHoldsWhenItIsSent(t *testing.T) {
	h := notification.NewRecheck(t)
	type person = notification.Person
	type commented struct {
		node        string // the doc, in the graph
		doc, remark uuid.UUID
	}
	// comment starts a private doc of from's shared for reading with to; then
	// text is said on it, emailed to to: from's comment, told as the inbox
	// tells a reply (business/Follow), or a guest's, through the doc's link.
	comment := func(t *testing.T, from person, to []person, text string, guest bool) commented {
		t.Helper()
		c := commented{doc: uuid.New(), remark: uuid.New()}
		var readers []map[string]any
		var ids []string
		var told []notification.NoticeRecipient
		for _, p := range to {
			readers = append(readers, map[string]any{"uid": p.UID})
			ids = append(ids, p.ID.String())
			told = append(told, notification.NoticeRecipient{UserUUID: p.ID.String(), Reason: "participant"})
		}
		c.node = h.DG.Mutate(t, map[string]any{"uid": "_:d", "dgraph.type": "Doc", "doc_uuid": c.doc.String(), "doc_title": "Launch notes",
			"doc_private": true, "doc_deleted_at": "0001-01-01T00:00:00Z", "doc_owner": map[string]any{"uid": from.UID},
			"doc_created_by": map[string]any{"uid": from.UID}, "doc_reading_users": readers})["d"]
		if guest {
			grant := uuid.New()
			h.Exec(t, `INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by) VALUES ($1, $2, 'doc', $3, 'comment', $4)`,
				grant, grant[:], c.doc.String(), from.ID)
			h.Exec(t, `INSERT INTO guest_comments (id, grant_id, doc_uuid, guest_name, body) VALUES ($1, $2, $3, 'Priya', $4)`, c.remark, grant, c.doc.String(), text)
			notification.DispatchDocComment("", "Priya (guest)", "", c.doc.String(), "Launch notes", text, c.remark.String(), ids)
		} else {
			h.Exec(t, `INSERT INTO comments (id, created_by, created_at) VALUES ($1, $2, NOW())`, c.remark, from.ID)
			notification.Tell(h.Ctx, notification.Notice{
				Kind: "thread.replied", ResourceKind: "doc", ResourceUUID: c.doc.String(), ResourceTitle: "Launch notes",
				Actor: &dgraphStruct.DgraphUser{Uuid: from.ID.String(), UserName: "Ada"}, DedupKey: "comment|" + c.remark.String(),
				Email: notification.EventDocComment, Title: "Ada replied in a thread on Launch notes", Body: text,
				Path:  "/app/doc/" + c.doc.String() + "/comment",
				About: &notification.About{Kind: notification.AboutDoc, Item: c.doc.String(), Reply: c.remark.String()},
			}, told)
		}
		h.Queued(t, to)
		return c
	}

	t.Run("a doc's comment is asked again: who was taken off it or whose grant ended is sent nothing", func(t *testing.T) {
		ada, bo, cy, di := h.Person(t, "Ada"), h.Person(t, "Bo"), h.Person(t, "Cy"), h.Person(t, "Di")
		c := comment(t, ada, []person{bo, cy, di}, "the launch notes need a date", false)
		if err := docBusiness.ApplySharing(h.Ctx, []string{c.doc.String()}, docBusiness.SharingChange{
			People: map[string]level.Level{cy.ID.String(): level.None}, Teams: map[string]level.Level{}}); err != nil {
			t.Fatal(err)
		}
		h.Exec(t, `INSERT INTO access_grant_expiry (resource_kind, resource_uuid, principal_kind, principal_uuid, level, expires_at)
			VALUES ('doc', $1, 'user', $2, 'view', NOW() - interval '1 minute')`, c.doc, di.ID)
		got := h.Send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the launch notes need a date") {
			t.Errorf("Bo, who still reads the doc, was sent %v, want one email quoting the comment", got[bo.Email])
		}
		h.NotTold(t, got, cy, "nothing quotes this")
		h.NotTold(t, got, di, "nothing quotes this")
	})

	t.Run("a doc's mention is asked again: who was taken off the doc is sent nothing", func(t *testing.T) {
		ada, bo, cy := h.Person(t, "Ada"), h.Person(t, "Bo"), h.Person(t, "Cy")
		c := comment(t, ada, []person{bo, cy}, "the launch notes are in review", false)
		h.Send(t) // the comment's own emails
		// A mention in the doc's words, as business/Follow tells it: of the
		// doc, with no reply.
		named := []string{bo.ID.String(), cy.ID.String()}
		notification.TellMentionsAdded(h.Ctx, notification.Notice{
			Kind: "doc.mentioned", ResourceKind: "doc", ResourceUUID: c.doc.String(), ResourceTitle: "Launch notes",
			Actor: &dgraphStruct.DgraphUser{Uuid: ada.ID.String(), UserName: "Ada"}, DedupKey: "doc.mentioned|" + c.doc.String(),
			Email: notification.EventDocMention, Title: "Ada mentioned you in Launch notes", Body: "the pricing table is final",
			Path:  "/app/doc/" + c.doc.String(),
			About: &notification.About{Kind: notification.AboutDoc, Item: c.doc.String(), Heard: notifylevel.Named},
		}, notification.MentionSave{Kind: "doc", ID: c.doc.String(), After: named,
			Hear:  func(_ context.Context, added []string) ([]string, error) { return added, nil },
			Named: func(string) bool { return true }})
		h.Queued(t, []person{bo, cy})
		if err := docBusiness.ApplySharing(h.Ctx, []string{c.doc.String()}, docBusiness.SharingChange{
			People: map[string]level.Level{cy.ID.String(): level.None}, Teams: map[string]level.Level{}}); err != nil {
			t.Fatal(err)
		}
		got := h.Send(t)
		if len(got[bo.Email]) != 1 || !strings.Contains(got[bo.Email][0].Text, "the pricing table is final") {
			t.Errorf("Bo, who still reads the doc, was sent %v, want the mention", got[bo.Email])
		}
		h.NotTold(t, got, cy, "nothing quotes this")
	})

	t.Run("a doc deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		c := comment(t, ada, []person{bo}, "the launch notes' last section", true)
		h.DG.Mutate(t, map[string]any{"uid": c.node, "doc_deleted_at": "2026-01-02T00:00:00Z"})
		h.NotTold(t, h.Send(t), bo, "the launch notes' last section")
	})

	t.Run("a doc comment deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		c := comment(t, ada, []person{bo}, "the launch notes' draft link", false)
		h.Exec(t, `UPDATE comments SET deleted_at = NOW() WHERE id = $1`, c.remark)
		h.NotTold(t, h.Send(t), bo, "the launch notes' draft link")
	})

	t.Run("a guest's comment deleted before the send is not quoted", func(t *testing.T) {
		ada, bo := h.Person(t, "Ada"), h.Person(t, "Bo")
		c := comment(t, ada, []person{bo}, "a guest asks about pricing", true)
		h.Exec(t, `UPDATE guest_comments SET deleted_at = NOW() WHERE id = $1`, c.remark)
		h.NotTold(t, h.Send(t), bo, "a guest asks about pricing")
	})
}
