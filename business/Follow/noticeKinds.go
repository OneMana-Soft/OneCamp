package business

// Notices about things other than docs, in the inbox (feed.go): a kind of
// resource that sends notices registers how its reader's inbox names them,
// from its package's init:
//
//	func init() { followBusiness.RegisterNoticeKind("table_row", rowNotices) }
//
// so a notice about it is titled, and opens, exactly when its reader may open
// the thing now (I12), as a doc's is. A kind nothing registered is shown as
// before: untitled, and opening nothing. This package imports none of them.
// A kind the Access Resolver titles registers TitledByAccess(kind).

import (
	"context"
	"fmt"
	"strings"
	"sync"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// NoticeKind answers, for a notice's reader, which of one kind's resources
// (by id) they may open now, and what each of those is called. It is asked for
// one page of the inbox at a time.
type NoticeKind func(ctx context.Context, readerUUID string, ids []string) (readable map[string]bool, titles map[string]string, err error)

var (
	noticeKindsMu sync.RWMutex
	noticeKinds   = map[string]NoticeKind{}
)

// RegisterNoticeKind adds how a kind's notices are named in the inbox. Docs
// are this package's own, and a second registration of one kind is a mistake
// two packages made in parallel: both panic where it happened.
func RegisterNoticeKind(kind string, k NoticeKind) {
	if kind == "" || kind == "doc" || k == nil {
		panic(fmt.Sprintf("follow: no notice kind can be registered as %q", kind))
	}
	noticeKindsMu.Lock()
	defer noticeKindsMu.Unlock()
	if _, taken := noticeKinds[kind]; taken {
		panic(fmt.Sprintf("follow: the notice kind %q is registered twice", kind))
	}
	noticeKinds[kind] = k
}

func noticeKindOf(kind string) (NoticeKind, bool) {
	noticeKindsMu.RLock()
	defer noticeKindsMu.RUnlock()
	k, ok := noticeKinds[kind]
	return k, ok
}

// noticeKey is a notice's resource as the feed's maps key it: a doc's id as it
// always was, and any other kind's id after its kind, so two kinds never
// share a key. Lower case. Pure.
func noticeKey(kind, id string) string {
	id = strings.ToLower(id)
	if kind == "doc" {
		return id
	}
	return kind + ":" + id
}

// otherNotices is what the registered kinds say of the inbox page's other
// notices, for its reader, keyed by noticeKey: which they may open and their
// titles. A kind whose answer can't be read leaves its notices untitled and
// unopened, as an unregistered kind's are.
func otherNotices(ctx context.Context, readerUUID string, byKind map[string][]string) (map[string]bool, map[string]string) {
	readable, titles := map[string]bool{}, map[string]string{}
	for kind, ids := range byKind {
		k, ok := noticeKindOf(kind)
		if !ok || len(ids) == 0 {
			continue
		}
		open, named, err := k(ctx, readerUUID, ids)
		if err != nil {
			continue
		}
		for id, yes := range open {
			if yes {
				readable[noticeKey(kind, id)] = true
			}
		}
		for id, title := range named {
			if open[id] {
				titles[noticeKey(kind, id)] = title
			}
		}
	}
	return readable, titles
}

// The Access Resolver's title reader, and its naming of a reader by graph
// uid: seams for tests.
var (
	readableTitles = accessBusiness.ReadableTitles
	readerUID      = accessBusiness.UIDOf
)

// TitledByAccess is how the inbox names the notices of a kind the Access
// Resolver titles (accessBusiness.ReadableTitles: an event, a project):
// titled, and opening, only while their reader may open the thing now. A
// reader it can't answer for is told nothing more than an untitled notice
// (fail closed).
func TitledByAccess(kind string) NoticeKind {
	return func(ctx context.Context, reader string, ids []string) (map[string]bool, map[string]string, error) {
		who := accessBusiness.Subject{UserUUID: reader}
		uid, err := readerUID(ctx, who)
		if err != nil {
			return nil, nil, err
		}
		who.UserUID = uid
		refs := make([]accessBusiness.ItemRef, 0, len(ids))
		for _, id := range ids {
			refs = append(refs, accessBusiness.ItemRef{Kind: kind, ID: id})
		}
		named, err := readableTitles(ctx, []accessBusiness.Subject{who}, refs)
		if err != nil {
			return nil, nil, err
		}
		readable, titles := make(map[string]bool, len(named)), make(map[string]string, len(named))
		for ref, title := range named {
			readable[ref.ID], titles[ref.ID] = true, title
		}
		return readable, titles, nil
	}
}
