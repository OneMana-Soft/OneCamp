package business

// A row's notices in the inbox (business/Follow noticeKinds.go): titled, and
// opening the row, only for a reader who may open it now; once they can't (the
// table made private, the row deleted) the notice keeps its place and loses its
// title, as a doc's does (I12).

import (
	"context"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
	"github.com/google/uuid"
)

func init() { followBusiness.RegisterNoticeKind(Kind, rowNotices) }

// noticesDecideAll is a seam for tests: what the reader may open.
var noticesDecideAll = accessBusiness.ResolveAll

// rowNotices is the inbox's reading of a page of row notices, for its reader:
// which rows they may open now, and what those are called ("Acme in Deals").
func rowNotices(ctx context.Context, readerUUID string, ids []string) (map[string]bool, map[string]string, error) {
	reader, err := uuid.Parse(readerUUID)
	if err != nil {
		return nil, nil, nil
	}
	account, err := notifyAccount(ctx, reader)
	if err != nil {
		return nil, nil, err
	}
	readable := map[string]bool{}
	titles := map[string]string{}
	if account == nil {
		return readable, titles, nil // deactivated: nothing opens
	}
	res := make([]accessBusiness.Resource, 0, len(ids))
	for _, id := range ids {
		if canonical(strings.ToLower(id)) {
			res = append(res, accessBusiness.Resource{Kind: Kind, UUID: strings.ToLower(id)})
		}
	}
	decided, err := noticesDecideAll(ctx, subjectOf(readerUUID, account), res)
	if err != nil {
		return nil, nil, err
	}
	var open []uuid.UUID
	for r, d := range decided {
		if !d.Deleted && d.Level >= accessBusiness.LevelView {
			readable[r.UUID] = true
			if parsed, err := uuid.Parse(r.UUID); err == nil {
				open = append(open, parsed)
			}
		}
	}
	if len(open) == 0 {
		return readable, titles, nil
	}
	names, err := notifyTitles(ctx, open)
	if err != nil {
		return nil, nil, err
	}
	for id, rt := range names {
		if !rt.Deleted {
			titles[id.String()] = place(rt)
		}
	}
	return readable, titles, nil
}
