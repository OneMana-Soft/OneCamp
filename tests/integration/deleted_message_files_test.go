//go:build integration
// +build integration

package integration_test

// A deleted message's files open for nobody: a channel post's, a conversation
// message's and a group message's, and their thread replies'. Deleting one
// takes the files it holds off with it, as taking a file off a task does:
// their rows are stamped deleted, so every read by id answers what a file that
// isn't there answers (404), and the purge takes their bytes. Deleting a
// message used to mark its files deleted in search only, and the purge never
// took them.
//
// A file a deleted message shares with one still there stays while a holder
// the server made shows it: a reply sent to its channel too is the reply and
// a copy holding the same file (linked by post_broadcast_of), and a message
// the file's uploader forwards within its place holds it as theirs. A message
// written since 19 Oct that merely names the file's id, someone else's or in
// another place, keeps nothing alive. One written before then holds what it
// shows, whoever wrote it: until then the server kept any file a request
// named, and a forward wrote its nodes with no adder or time.
//
// setUpPlacesForFiles: Mia's private finance channel (Ann reads it too here),
// her conversation with Ann, and the group of Mia, Ann and Ben. Mia uploaded
// every file and wrote every message but Ann's two in the group.
//
// Run: go test -tags=integration ./tests/integration/ -run '^(TestADeletedMessagesFilesOpenForNobody|TestARepairTakesTheFilesOfMessagesDeletedBefore)$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/helpers"
)

// A heldFile is a file of its place (the attachments row's src_key and
// src_value), as the app uploads it.
type heldFile struct{ name, id, kind, place string }

// filesWorld is setUpPlacesForFiles with messages that hold files, and the
// ways to ask for a file and to delete a message as the app does.
type filesWorld struct {
	*placesForFiles
	t     *testing.T
	ids   map[string]string // each message's id, by its name in the test
	graph map[string]string // and its node's uid
	// Mia's files: the budget post's and its four replies', the invoices
	// post's, the contract message's and its reply's, the notes message's, the
	// roster's in the group, the agenda her forward from before 19 Oct carries
	// to two places, and the minutes Ann's message from then names.
	budget, firstDraft, secondDraft, thirdDraft, fourthDraft, invoices heldFile
	contract, signed, notes, roster, agenda, minutes                   heldFile
	all                                                                []heldFile
}

// written is when and by whom the server wrote a message's file nodes. Since
// 19 Oct it writes each itself, with the writer as its adder and the time
// (models/dgraph NewAttachments); a forward before then wrote neither.
type written struct {
	adder map[string]any
	at    string // "" for nodes written with no time
}

func setUpFilesWorld(t *testing.T) *filesWorld {
	t.Helper()
	p := setUpPlacesForFiles(t)
	w := &filesWorld{placesForFiles: p, t: t, ids: map[string]string{}}
	mia := p.people["mia"]
	inChannel := func(name string) heldFile { return heldFile{name, uuid.NewString(), "channel", p.channel} }
	inDM := func(name string) heldFile { return heldFile{name, uuid.NewString(), "chat", p.dmMiaAnn} }
	w.budget, w.firstDraft, w.secondDraft = inChannel("q3-budget.xlsx"), inChannel("first-draft.pdf"), inChannel("second-draft.pdf")
	w.thirdDraft, w.fourthDraft, w.invoices = inChannel("third-draft.pdf"), inChannel("fourth-draft.pdf"), inChannel("invoices.zip")
	w.contract, w.signed, w.notes = inDM("contract.pdf"), inDM("signed.pdf"), inDM("notes.txt")
	w.roster = heldFile{"roster.png", uuid.NewString(), "grpChat", p.group}
	w.agenda, w.minutes = inChannel("agenda.pdf"), inChannel("minutes.pdf")
	w.all = []heldFile{w.budget, w.firstDraft, w.secondDraft, w.thirdDraft, w.fourthDraft, w.invoices, w.contract, w.signed,
		w.notes, w.roster, w.agenda, w.minutes}
	for _, f := range w.all {
		w.exec(`INSERT INTO attachments (id, obj_key, src_key, src_value, created_by) VALUES ($1, $2, $3, $4, $5)`,
			f.id, mia.String()+"/"+f.id+"_"+f.name, f.kind, f.place, mia)
	}
	for _, name := range []string{"budgetPost", "reply1", "reply2", "reply3", "reply4", "copy1", "copy2", "copy3", "copy4",
		"invoicePost", "invoiceForward", "contractMsg", "signedReply", "notesMsg", "rosterMsg", "annsMention",
		"agendaForward", "agendaForwardToAnn", "minutesPost", "annsOldMention"} {
		w.ids[name] = uuid.NewString()
	}

	live, now := "0001-01-01T00:00:00Z", time.Now().UTC().Format(time.RFC3339)
	ref := func(uid string) map[string]any { return map[string]any{"uid": uid} }
	byMia, byAnn := ref(p.uids["mia"]), ref(p.uids["ann"])
	miaNow, annNow := written{byMia, now}, written{byAnn, now}
	// Before 19 Oct: a forward's nodes with neither adder nor time, and Ann's
	// message of 15 Oct, written as the server wrote a message's then.
	untimed, annOct15 := written{}, written{byAnn, "2026-10-15T09:00:00Z"}
	nodes := func(by written, fs ...heldFile) []map[string]any {
		out := make([]map[string]any, 0, len(fs))
		for _, f := range fs {
			n := map[string]any{"dgraph.type": "Attachment", "attachment_uuid": f.id, "attachment_file_name": f.name,
				"attachment_obj_key": mia.String() + "/" + f.id + "_" + f.name, "attachment_deleted_at": live}
			if by.adder != nil {
				n["attachment_created_by"] = by.adder
			}
			if by.at != "" {
				n["attachment_created_at"] = by.at
			}
			out = append(out, n)
		}
		return out
	}
	// An item written with untimed nodes was written before 19 Oct.
	day := func(by written) string {
		if by.at == "" {
			return "2026-10-12T09:00:00Z"
		}
		return by.at
	}
	post := func(name string, by written, fs ...heldFile) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "Post", "post_uuid": w.ids[name], "post_by": byMia,
			"post_channel": ref(p.uids["finance"]), "post_created_at": day(by), "post_deleted_at": live, "post_attachments": nodes(by, fs...)}
	}
	reply := func(name, on, edge string, by written, f heldFile) map[string]any {
		return map[string]any{"uid": "_:" + name, "dgraph.type": "Comment", "comment_uuid": w.ids[name], "comment_by": byMia,
			edge: ref("_:" + on), "comment_created_at": day(by), "comment_deleted_at": live, "comment_attachments": nodes(by, f)}
	}
	// A reply sent to the channel too: a copy holding the reply's file, linked
	// to the reply, as the app posts it (postController.CreateCommentInPost).
	copyOf := func(name, of string, by written, f heldFile) map[string]any {
		c := post(name, by, f)
		c["post_broadcast_of"] = ref("_:" + of)
		return c
	}
	msg := func(name, dm string, from map[string]any, by written, fs []heldFile, replies ...map[string]any) map[string]any {
		m := map[string]any{"uid": "_:" + name, "dgraph.type": "Chat", "chat_uuid": w.ids[name], "chat_from": from,
			"chat_dm": ref(dm), "chat_created_at": day(by), "chat_deleted_at": live, "chat_attachments": nodes(by, fs...)}
		if len(replies) > 0 {
			m["chat_comments"] = replies
		}
		return m
	}
	budgetPost := post("budgetPost", miaNow, w.budget)
	budgetPost["post_comments"] = []map[string]any{
		reply("reply1", "budgetPost", "comment_post", miaNow, w.firstDraft),
		reply("reply2", "budgetPost", "comment_post", miaNow, w.secondDraft),
		reply("reply3", "budgetPost", "comment_post", miaNow, w.thirdDraft),
		// Sent to the channel too before the server set a file's adder.
		reply("reply4", "budgetPost", "comment_post", untimed, w.fourthDraft),
	}
	w.graph = p.dg.Mutate(t, []map[string]any{
		{"uid": p.uids["finance"], "ch_members": []map[string]any{byAnn}},
		budgetPost,
		copyOf("copy1", "reply1", miaNow, w.firstDraft), copyOf("copy2", "reply2", miaNow, w.secondDraft),
		copyOf("copy3", "reply3", miaNow, w.thirdDraft), copyOf("copy4", "reply4", untimed, w.fourthDraft),
		post("invoicePost", miaNow, w.invoices),
		// Mia forwarding her invoices within the channel: her file, as hers.
		post("invoiceForward", miaNow, w.invoices),
		{"uid": "_:dm", "dgraph.type": "Dm", "dm_grouping_id": p.dmMiaAnn, "dm_participants": []map[string]any{byMia, byAnn}},
		msg("contractMsg", "_:dm", byMia, miaNow, []heldFile{w.contract}, reply("signedReply", "contractMsg", "comment_chat", miaNow, w.signed)),
		msg("notesMsg", "_:dm", byMia, miaNow, []heldFile{w.notes}),
		msg("rosterMsg", p.uids["crew"], byMia, miaNow, []heldFile{w.roster}),
		// Ann naming the budget's file in the group: someone else's file, in
		// another place, which no write of the app keeps since 19 Oct
		// (business/Attachment OwnFiles). It holds nothing.
		msg("annsMention", p.uids["crew"], byAnn, annNow, []heldFile{w.budget}),
		// Mia's forward of 12 Oct to the channel and to Ann at once, with the
		// agenda added in the forward box: one row, a node in each place.
		post("agendaForward", untimed, w.agenda),
		msg("agendaForwardToAnn", "_:dm", byMia, untimed, []heldFile{w.agenda}),
		// Ann's group message of 15 Oct naming Mia's minutes, which the server
		// then kept as the request named them.
		post("minutesPost", miaNow, w.minutes),
		msg("annsOldMention", p.uids["crew"], byAnn, annOct15, []heldFile{w.minutes}),
	})
	w.exec(`INSERT INTO channels (id, ch_name, ch_private, created_by) VALUES ($1, 'finance', true, $2)`, p.channel, mia)
	for _, name := range []string{"budgetPost", "copy1", "copy2", "copy3", "copy4", "invoicePost", "invoiceForward", "agendaForward", "minutesPost"} {
		w.exec(`INSERT INTO posts (id, post_channel, created_by) VALUES ($1, $2, $3)`, w.ids[name], p.channel, mia)
	}
	for _, name := range []string{"reply1", "reply2", "reply3", "reply4", "signedReply"} {
		w.exec(`INSERT INTO comments (id, created_by) VALUES ($1, $2)`, w.ids[name], mia)
	}
	for name, c := range map[string]struct {
		group string
		by    uuid.UUID
	}{"contractMsg": {p.dmMiaAnn, mia}, "notesMsg": {p.dmMiaAnn, mia}, "rosterMsg": {p.group, mia}, "annsMention": {p.group, p.people["ann"]},
		"agendaForwardToAnn": {p.dmMiaAnn, mia}, "annsOldMention": {p.group, p.people["ann"]}} {
		w.exec(`INSERT INTO chats (id, grp_id, created_by) VALUES ($1, $2, $3)`, w.ids[name], c.group, c.by)
	}
	return w
}

func (w *filesWorld) exec(q string, args ...any) {
	w.t.Helper()
	if _, err := w.env.PG.Exec(q, args...); err != nil {
		w.t.Fatal(err)
	}
}

// serve runs handler h as who.
func (w *filesWorld) serve(who string, h http.HandlerFunc, method, target string, params map[string]string, body any) (int, string) {
	w.t.Helper()
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	ctx := context.WithValue(context.WithValue(context.Background(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, w.as(who))
	raw, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(method, target, bytes.NewReader(raw)).WithContext(ctx))
	out := rec.Body.String()
	if len(out) > 200 {
		out = out[:200]
	}
	return rec.Code, out
}

// opening is a person asking for a file through its place's route: a
// conversation's through the other one in it, as the app names it.
func (w *filesWorld) opening(who string, f heldFile, query string) (int, string) {
	w.t.Helper()
	switch f.kind {
	case "channel":
		return w.serve(who, userController.GetChannelFile, http.MethodGet, "/"+query, map[string]string{"channel_uuid": w.channel, "obj_uuid": f.id}, nil)
	case "chat":
		other := w.people["ann"]
		if who == "ann" {
			other = w.people["mia"]
		}
		return w.serve(who, userController.GetChatFile, http.MethodGet, "/"+query, map[string]string{"chat_uuid": other.String(), "obj_uuid": f.id}, nil)
	}
	return w.serve(who, userController.GetGroupChatFile, http.MethodGet, "/"+query, map[string]string{"grp_id": w.group, "obj_uuid": f.id}, nil)
}

// opens and gone check files for their uploader and another reader, as a
// link and as the picture a page shows (?redirect=1).
func (w *filesWorld) opens(when string, fs ...heldFile) {
	w.t.Helper()
	for _, f := range fs {
		for _, who := range []string{"mia", "ann"} {
			if code, body := w.opening(who, f, ""); code != http.StatusOK {
				w.t.Errorf("%s: %s opening %s: %d %s", when, who, f.name, code, body)
			}
			if code, body := w.opening(who, f, "?redirect=1"); code != http.StatusFound {
				w.t.Errorf("%s: %s opening %s?redirect=1: %d %s", when, who, f.name, code, body)
			}
		}
	}
}

func (w *filesWorld) gone(when string, fs ...heldFile) {
	w.t.Helper()
	for _, f := range fs {
		for _, who := range []string{"mia", "ann"} {
			for _, query := range []string{"", "?redirect=1"} {
				if code, body := w.opening(who, f, query); code != http.StatusNotFound {
					w.t.Errorf("%s: %s opened %s%s: %d %s", when, who, f.name, query, code, body)
				}
			}
		}
	}
}

// stamped checks which rows are stamped deleted, the ones the purge takes:
// every file but those kept.
func (w *filesWorld) stamped(kept ...heldFile) {
	w.t.Helper()
	keep := map[string]bool{}
	for _, f := range kept {
		keep[f.id] = true
	}
	for _, f := range w.all {
		var stamped bool
		if err := w.env.PG.QueryRow(`SELECT deleted_at IS NOT NULL FROM attachments WHERE id = $1`, f.id).Scan(&stamped); err != nil {
			w.t.Fatal(err)
		}
		if stamped == keep[f.id] {
			w.t.Errorf("%s's row stamped deleted: %v, want %v", f.name, stamped, !keep[f.id])
		}
	}
}

// deleting is who deleting a message through the app's route.
func (w *filesWorld) deleting(who, what string, h http.HandlerFunc, body any) {
	w.t.Helper()
	if code, out := w.serve(who, h, http.MethodPost, "/", nil, body); code != http.StatusOK {
		w.t.Fatalf("%s deleting %s: %d %s", who, what, code, out)
	}
}

func TestADeletedMessagesFilesOpenForNobody(t *testing.T) {
	w := setUpFilesWorld(t)
	id := w.ids

	w.opens("before", w.all...)

	// A reply's copy alone: the reply in the thread still shows its file,
	// a copy from before the server set a file's adder as well.
	w.deleting("mia", "the first reply's copy in the channel", postController.DeletePost, map[string]string{"post_id": id["copy1"]})
	w.deleting("mia", "the fourth reply's copy in the channel", postController.DeletePost, map[string]string{"post_id": id["copy4"]})
	w.opens("the copies deleted", w.firstDraft, w.fourthDraft, w.budget)

	// The second reply: its copy goes with it, and so does its file.
	w.deleting("mia", "the second reply", postController.DeleteCommentInPost, map[string]string{"comment_id": id["reply2"], "post_id": id["budgetPost"]})
	w.gone("the second reply deleted", w.secondDraft)
	w.opens("the second reply deleted", w.budget, w.firstDraft, w.thirdDraft, w.fourthDraft)

	// The post: its file and its replies' go, though Ann's message of today
	// names the budget's id; the third reply's stays, its copy still showing it.
	w.deleting("mia", "the budget post", postController.DeletePost, map[string]string{"post_id": id["budgetPost"]})
	w.gone("the post deleted", w.budget, w.firstDraft, w.fourthDraft)
	w.opens("the post deleted", w.thirdDraft, w.invoices)

	// The invoices post: Mia's forward within the channel still shows them.
	w.deleting("mia", "the invoices post", postController.DeletePost, map[string]string{"post_id": id["invoicePost"]})
	w.opens("the invoices post deleted", w.invoices)

	// A message of the conversation, with its reply's file; the next message's stays.
	w.deleting("mia", "the contract message", chatController.DeleteChat, map[string]string{"chat_id": id["contractMsg"]})
	w.gone("the contract message deleted", w.contract, w.signed)
	w.opens("the contract message deleted", w.notes)

	// A group's message.
	w.deleting("mia", "the roster message", chatController.DeleteChat, map[string]string{"chat_id": id["rosterMsg"]})
	w.gone("the roster message deleted", w.roster)

	// A forward from before 19 Oct, to two places at once: deleting its copy
	// in one leaves the file with the other; deleting both takes it.
	w.deleting("mia", "the agenda's forward in the channel", postController.DeletePost, map[string]string{"post_id": id["agendaForward"]})
	w.opens("one of the agenda's forwards deleted", w.agenda)
	w.deleting("mia", "the agenda's forward to Ann", chatController.DeleteChat, map[string]string{"chat_id": id["agendaForwardToAnn"]})
	w.gone("both of the agenda's forwards deleted", w.agenda)

	// A message from before then naming someone else's file holds it too, as
	// the server kept it: Ann's of 15 Oct keeps Mia's minutes after Mia's post
	// goes, until it goes as well.
	w.deleting("mia", "the minutes post", postController.DeletePost, map[string]string{"post_id": id["minutesPost"]})
	w.opens("the minutes post deleted", w.minutes)
	w.deleting("ann", "her message of 15 Oct naming the minutes", chatController.DeleteChat, map[string]string{"chat_id": id["annsOldMention"]})
	w.gone("both messages showing the minutes deleted", w.minutes)

	w.stamped(w.thirdDraft, w.invoices, w.notes)
}

// Messages deleted before a delete took their files with them still let the
// people of their place open those files, and the purge never takes them. A
// repair each install runs once (business/Attachment DeletedFilesRepair)
// takes them as a delete does now, by the same rule, and puts them in the
// trash from its own run, never earlier: their bytes go after the normal
// retention, not at once.
func TestARepairTakesTheFilesOfMessagesDeletedBefore(t *testing.T) {
	w := setUpFilesWorld(t)
	id := w.ids
	// Deleted as the app did before: the item marked deleted in the graph and
	// in Postgres, its file nodes and rows left as they were.
	deletedBefore := func(kind, name string) {
		t.Helper()
		w.dg.Mutate(t, []map[string]any{{"uid": w.graph[name], kind + "_deleted_at": "2026-10-20T10:00:00Z"}})
		w.exec(`UPDATE `+kind+`s SET deleted_at = '2026-10-20T10:00:00Z' WHERE id = $1`, id[name])
	}
	deletedBefore("post", "budgetPost") // Ann's message of today naming the budget keeps nothing
	deletedBefore("comment", "reply2")  // its copy in the channel still shows the second draft
	deletedBefore("chat", "contractMsg")
	deletedBefore("post", "agendaForward") // its other copy, from before 19 Oct, still shows the agenda
	// Deleted in Postgres alone, a delete that stopped half way: the app still
	// shows the message, so its file stays.
	w.exec(`UPDATE chats SET deleted_at = '2026-10-20T10:00:00Z' WHERE id = $1`, id["notesMsg"])

	w.opens("before the repair", w.all...)

	ran := time.Now().Add(-time.Second)
	rep, did, err := attachmentBusiness.DeletedFilesRepair.Run(context.Background())
	if err != nil || !did {
		t.Fatalf("the repair: ran %v, %v", did, err)
	}
	taken := []heldFile{w.budget, w.contract, w.signed}
	w.gone("after the repair", taken...)
	w.opens("after the repair", w.firstDraft, w.secondDraft, w.thirdDraft, w.fourthDraft, w.invoices, w.notes, w.roster, w.agenda, w.minutes)
	w.stamped(slices.DeleteFunc(slices.Clone(w.all), func(f heldFile) bool { return slices.Contains(taken, f) })...)
	var early int
	if err := w.env.PG.QueryRow(`SELECT count(*) FROM attachments WHERE deleted_at < $1`, ran).Scan(&early); err != nil || early != 0 {
		t.Errorf("%d rows stamped before the repair ran (%v): the purge would take them before their retention", early, err)
	}
	if want := (attachmentBusiness.DeletedFilesReport{Posts: 2, Chats: 1, Comments: 1, Files: 3}); rep != want {
		t.Errorf("the repair did %+v, want %+v", rep, want)
	}
	var audited string
	if err := w.env.PG.QueryRow(`SELECT metadata->>'files' FROM admin_audit_log WHERE action = 'files.deleted_messages_repaired'`).Scan(&audited); err != nil || audited != "3" {
		t.Errorf("the audit log's record of the repair: %q (%v), want 3 files", audited, err)
	}

	// Once: a second start finds it done and reads nothing.
	if _, did, err := attachmentBusiness.DeletedFilesRepair.Run(context.Background()); err != nil || did {
		t.Fatalf("the repair again: ran %v, %v", did, err)
	}
}
