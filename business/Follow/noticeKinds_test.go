package business

import (
	"context"
	"errors"
	"testing"

	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

// A kind other than docs names its notices through what it registers: titled
// and opening only where its reader may open the thing now, never by another
// kind's answer, and untitled when its answer can't be read.
func TestOtherKindsNameTheirNoticesForTheirReader(t *testing.T) {
	const kind = "test_kind_feed"
	open, shut := "aa000000-0000-4000-8000-000000000001", "aa000000-0000-4000-8000-000000000002"
	failing := false
	RegisterNoticeKind(kind, func(_ context.Context, reader string, ids []string) (map[string]bool, map[string]string, error) {
		if failing {
			return nil, nil, errors.New("down")
		}
		readable, titles := map[string]bool{}, map[string]string{}
		for _, id := range ids {
			readable[id] = reader == ownerID && id == open
			titles[id] = "Acme in Deals"
		}
		return readable, titles, nil
	})
	t.Cleanup(func() {
		noticeKindsMu.Lock()
		delete(noticeKinds, kind)
		noticeKindsMu.Unlock()
	})
	rows := []*followModels.Notice{
		{ID: uuid.New(), Kind: "table_row.mentioned", ResourceKind: kind, ResourceUUID: open, CreatedAt: noon},
		{ID: uuid.New(), Kind: "table_row.mentioned", ResourceKind: kind, ResourceUUID: shut, CreatedAt: noon},
		{ID: uuid.New(), Kind: "doc.mentioned", ResourceKind: "doc", ResourceUUID: open, CreatedAt: noon},
	}
	readable, titles := otherNotices(context.Background(), ownerID, map[string][]string{kind: {open, shut}})
	items := noticeItems(rows, readable, titles, nil)
	if n := items[0].Notice; !n.CanOpen || n.Title != "Acme in Deals" {
		t.Errorf("a notice its reader may open: %+v", n)
	}
	if n := items[1].Notice; n.CanOpen || n.Title != "" {
		t.Errorf("a notice its reader may not open: %+v", n)
	}
	// A doc with the same id isn't opened by the other kind's answer.
	if n := items[2].Notice; n.CanOpen || n.Title != "" {
		t.Errorf("a doc opened by another kind's answer: %+v", n)
	}
	failing = true
	readable, titles = otherNotices(context.Background(), ownerID, map[string][]string{kind: {open}})
	if n := noticeItems(rows[:1], readable, titles, nil)[0].Notice; n.CanOpen || n.Title != "" {
		t.Errorf("an answer that couldn't be read still names it: %+v", n)
	}
	// Docs are this package's own, and no kind registers twice.
	for _, c := range []struct {
		name string
		kind string
	}{{"docs", "doc"}, {"twice", kind}, {"no name", ""}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("registering %s didn't panic", c.name)
				}
			}()
			RegisterNoticeKind(c.kind, func(context.Context, string, []string) (map[string]bool, map[string]string, error) {
				return nil, nil, nil
			})
		}()
	}
}
