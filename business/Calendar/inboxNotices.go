package Calendar

// The inbox names a calendar notice (invited, changed, cancelled, answered,
// a reminder) and opens it only while its reader may open the event now, as
// it does a doc's and a table row's (business/Follow noticeKinds.go): the
// Access Resolver's titles say which, and a reader it can't answer for is
// told nothing more than an untitled notice (fail closed).

import (
	"context"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	followBusiness "github.com/akashc777/OneCamp/business/Follow"
)

func init() { followBusiness.RegisterNoticeKind(KindEvent, eventNotices) }

// readableTitles is the Access Resolver's title reader: a seam for tests.
var readableTitles = accessBusiness.ReadableTitles

// eventNotices is which of the events named (by id) reader may open now, and
// what each of those is called.
func eventNotices(ctx context.Context, reader string, ids []string) (map[string]bool, map[string]string, error) {
	refs := make([]accessBusiness.ItemRef, 0, len(ids))
	for _, id := range ids {
		refs = append(refs, accessBusiness.ItemRef{Kind: KindEvent, ID: id})
	}
	named, err := readableTitles(ctx, []accessBusiness.Subject{{UserUUID: reader}}, refs)
	if err != nil {
		return nil, nil, err
	}
	readable, titles := make(map[string]bool, len(named)), make(map[string]string, len(named))
	for ref, title := range named {
		readable[ref.ID], titles[ref.ID] = true, title
	}
	return readable, titles, nil
}
