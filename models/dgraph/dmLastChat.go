package models

// A DM's last time (dm_last_chat_at) is when its newest message was written.
// The DM list reads newest first, a page at a time, by it (DG4). Every write
// that adds messages to a DM moves the time forward in the same request, and
// never back: a backdated import or a shared history's copy keeps its older
// time, and a DM with a newer message keeps that message's time.

import (
	"fmt"
	"strings"
	"time"

	"github.com/dgraph-io/dgo/v230/protos/api"
)

// NewestChatAt is the newest time among the messages a write adds to dm.
func NewestChatAt(dm *DgraphDm) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, c := range dm.Chats {
		if c != nil && c.CreatedAt != nil && (!found || c.CreatedAt.After(newest)) {
			newest, found = *c.CreatedAt, true
		}
	}
	return newest, found
}

// StampLastChats adds to req, an upsert writing dms, what moves each DM's last
// time forward to its newest new message: a query block naming the DM when its
// time is already at or past that, and a mutation, conditional on it not
// being, that sets the time. req's query must be one block ("query { ... }")
// that defines each DM's variable, as every chat write's does.
func StampLastChats(req *api.Request, dms ...*DgraphDm) {
	var blocks []string
	for i, dm := range dms {
		at, hasChats := NewestChatAt(dm)
		fn, subject, named := dmNode(dm.Uid)
		if !hasChats || !named {
			continue
		}
		ts := at.UTC().Format(time.RFC3339Nano)
		name := fmt.Sprintf("dm_last_newer_%d", i)
		blocks = append(blocks, fmt.Sprintf(`%s as var(func: %s) @filter(ge(dm_last_chat_at, %q))`, name, fn, ts))
		req.Mutations = append(req.Mutations, &api.Mutation{
			Cond:      fmt.Sprintf("@if(eq(len(%s), 0))", name),
			SetNquads: []byte(fmt.Sprintf(`%s <dm_last_chat_at> %q^^<xs:dateTime> .`, subject, ts)),
		})
	}
	if len(blocks) == 0 {
		return
	}
	end := strings.LastIndex(req.Query, "}")
	req.Query = req.Query[:end] + "\t" + strings.Join(blocks, "\n\t") + "\n" + req.Query[end:]
}

// dmNode is how an upsert names a DM, as a function and as a subject: by its
// variable (uid(dm)) or by its uid.
func dmNode(uid string) (fn, subject string, ok bool) {
	if v, found := strings.CutPrefix(uid, "uid("); found && strings.HasSuffix(v, ")") {
		return uid, uid, true
	}
	if strings.HasPrefix(uid, "0x") {
		return "uid(" + uid + ")", "<" + uid + ">", true
	}
	return "", "", false
}
