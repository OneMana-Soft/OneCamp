package business

// The inbox's reading of notices (business/Activity's NoticeSource): a
// person's notices, newest first, each with the thing's title and the people in
// it read now. A notice about a doc its reader can no longer open keeps its
// place but loses its title (I12), and can't be opened from the inbox. Whether
// they can open it is the Access Resolver's answer for them (ResolveAll, one
// read for the page's docs), so a doc that reaches them through a team or its
// space is named, and one in the trash isn't.

import (
	"context"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Activity"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

// Seams for tests: the notices, which docs their reader may open now, and
// the titles of those they may.
var (
	noticesFor   = followModels.NoticesFor
	docsReadable = func(ctx context.Context, readerUUID string, docUUIDs []string) (map[string]bool, error) {
		res := make([]accessBusiness.Resource, 0, len(docUUIDs))
		for _, id := range docUUIDs {
			res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: id})
		}
		decided, err := accessBusiness.ResolveAll(ctx, accessBusiness.Subject{UserUUID: readerUUID}, res)
		if err != nil {
			return nil, err
		}
		out := make(map[string]bool, len(decided))
		for r, d := range decided {
			out[strings.ToLower(r.UUID)] = d.Level >= accessBusiness.LevelView
		}
		return out, nil
	}
	docTitles = docDomain.DocTitles
)

// unreadNotices is LastSeenActivity's UnreadNotices: notices since the person
// last looked at their inbox.
func unreadNotices(ctx context.Context, userUUID string, since time.Time) (uint64, error) {
	me, err := uuid.Parse(userUUID)
	if err != nil {
		return 0, nil
	}
	return followModels.CountNoticesSince(ctx, me, since)
}

// feedNotices is NoticeSource.
func feedNotices(ctx context.Context, userUUID string, before time.Time, limit int) ([]dgraphModels.UnifiedActivityItem, error) {
	me, err := uuid.Parse(userUUID)
	if err != nil {
		return nil, nil
	}
	rows, err := noticesFor(ctx, me, before, limit)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var docIDs, personIDs []string
	others := map[string][]string{}
	for _, n := range rows {
		if n.ResourceKind == "doc" {
			docIDs = append(docIDs, n.ResourceUUID)
		} else if n.ResourceKind != "" {
			others[n.ResourceKind] = append(others[n.ResourceKind], n.ResourceUUID)
		}
		personIDs = append(personIDs, n.ActorUUID, n.AgentUUID)
	}
	// Which docs the reader may open, and the titles of those: when either
	// can't be read, the items are still shown, untitled and unopened.
	readable, err := docsReadable(ctx, userUUID, docIDs)
	if err != nil {
		readable = nil
	}
	var open []string
	for _, id := range docIDs {
		if readable[strings.ToLower(id)] {
			open = append(open, id)
		}
	}
	titles, err := docTitles(ctx, open)
	if err != nil {
		readable, titles = nil, nil
	}
	// The other kinds' notices, by what each kind says (noticeKinds.go).
	if len(others) > 0 {
		open, named := otherNotices(ctx, userUUID, others)
		if readable == nil {
			readable = map[string]bool{}
		}
		if titles == nil {
			titles = map[string]string{}
		}
		for k, v := range open {
			readable[k] = v
		}
		for k, v := range named {
			titles[k] = v
		}
	}
	byID := map[string]*dgraphStruct.DgraphUser{}
	if who, err := people(ctx, personIDs); err == nil {
		for _, u := range who {
			if u != nil {
				byID[u.Uuid] = u
			}
		}
	}
	return noticeItems(rows, readable, titles, byID), nil
}

// noticeItems are notices as the feed lists them, for their reader: readable
// is what they may open now and titles what those are called, both keyed by
// noticeKey (a doc by its lower-case uuid). Pure.
func noticeItems(rows []*followModels.Notice, readable map[string]bool, titles map[string]string, byID map[string]*dgraphStruct.DgraphUser) []dgraphModels.UnifiedActivityItem {
	out := make([]dgraphModels.UnifiedActivityItem, 0, len(rows))
	for _, n := range rows {
		key := noticeKey(n.ResourceKind, n.ResourceUUID)
		out = append(out, notificationBusiness.NoticeItem(n, byID[n.ActorUUID], byID[n.AgentUUID], titles[key], readable[key]))
	}
	return out
}
