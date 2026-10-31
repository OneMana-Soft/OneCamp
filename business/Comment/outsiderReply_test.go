package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A reply by someone outside the conversation (let into its call by the
// call's invite, calls CALLS-3b) records the reply in the thread's index but
// not its writer, so the thread never enters their Threads (CHAT-LEAD-2's
// condition 2). Anyone else's reply records them, as always.
func TestAnOutsidersReplyRecordsNoReplier(t *testing.T) {
	post := &dgraphStruct.DgraphPost{Uuid: "11111111-1111-4111-8111-111111111111", PostBy: &dgraphStruct.DgraphUser{Uuid: "u-maya"}, Channel: &dgraphStruct.DgraphChannel{Uuid: "c1"}}
	at := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	mentioned := []*dgraphStruct.DgraphUser{{Uuid: "u-ben"}}
	jonas := &dgraphStruct.DgraphUser{Uuid: "u-jonas"}
	out := replyRecord(ThreadReply{By: jonas, Post: post, Mentioned: mentioned, At: at, Outsider: true})
	if out.By != "" || out.Thread.Root != post.Uuid || len(out.Mentioned) != 1 || !out.At.Equal(at) {
		t.Errorf("an outsider's reply: %+v", out)
	}
	if in := replyRecord(ThreadReply{By: jonas, Post: post, At: at}); in.By != "u-jonas" {
		t.Errorf("a member's reply: %+v", in)
	}
}
