//go:build integration
// +build integration

package integration_test

// A Slack workspace read through Slack's Web API comes across as its export
// does (chat item 2): a team on Slack's free or Pro plan exports its public
// channels only, so its private channels, DMs and group DMs come across this
// way. The fake Slack here serves the acme export's workspace through the API,
// as Slack answers Ada's token, two items a page so every cursor is followed,
// with one of her messages carrying a file only the token opens and one of a
// file shared from Google Drive. The read is asked for through the route; it
// streams its export into the store; and the job is planned and run through
// the routes. Then everything the export's own test checks holds, the file is
// there, and the token is nowhere it was kept.
//
// Run: go test -tags=integration ./tests/integration/ -run TestASlackWorkspaceReadThroughTheAPIComesAcross -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	slackImportController "github.com/akashc777/OneCamp/controllers/SlackImport"
	slackapi "github.com/akashc777/OneCamp/services/Slack"
	"github.com/akashc777/OneCamp/tests/integration"
)

// slackFromExport serves an export's workspace through Slack's Web API, as
// Slack answers self's token: the methods the import's API reader asks. Files
// are served at their paths, to the token alone.
type slackFromExport struct {
	token   string
	self    string
	users   []any
	kinds   map[string][]any // conversations.list, by type
	members map[string][]any
	pins    map[string][]any
	history map[string][]any // newest first
	replies map[string][]any // by thread ts, the root first
	byTs    map[string]map[string]any
	files   map[string][]byte // by path

	mu    sync.Mutex
	asked []string
}

func newSlackFromExport(t *testing.T, dir, token, self string) *slackFromExport {
	t.Helper()
	read := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	s := &slackFromExport{token: token, self: self, kinds: map[string][]any{}, members: map[string][]any{}, pins: map[string][]any{},
		history: map[string][]any{}, replies: map[string][]any{}, byTs: map[string]map[string]any{}, files: map[string][]byte{}}
	read("users.json", &s.users)
	// A conversation as conversations.list gives it (no people, no pins: the
	// reader asks for those), its people and pins, and its days' messages.
	add := func(kind, folder string, c map[string]any) {
		id := c["id"].(string)
		listed := map[string]any{}
		for k, v := range c {
			if k != "members" && k != "pins" {
				listed[k] = v
			}
		}
		s.kinds[kind] = append(s.kinds[kind], listed)
		if m, ok := c["members"].([]any); ok {
			s.members[id] = m
		}
		if ps, ok := c["pins"].([]any); ok {
			for _, p := range ps {
				pin := p.(map[string]any)
				s.pins[id] = append(s.pins[id], map[string]any{"type": "message", "channel": id, "created": pin["created"], "created_by": pin["user"], "message": map[string]any{"ts": pin["id"]}})
			}
		}
		days, err := os.ReadDir(filepath.Join(dir, folder))
		if err != nil {
			t.Fatal(err)
		}
		var top []any
		for _, d := range days {
			var msgs []map[string]any
			read(filepath.Join(folder, d.Name()), &msgs)
			for _, m := range msgs {
				ts, _ := m["ts"].(string)
				thread, _ := m["thread_ts"].(string)
				s.byTs[ts] = m
				if thread == "" || thread == ts || m["subtype"] == "thread_broadcast" {
					top = append(top, m)
				}
				if thread != "" {
					s.replies[thread] = append(s.replies[thread], m)
				}
			}
		}
		for i := len(top) - 1; i >= 0; i-- {
			s.history[id] = append(s.history[id], top[i])
		}
	}
	var channels, groups, dms, mpims []map[string]any
	read("channels.json", &channels)
	read("groups.json", &groups)
	read("dms.json", &dms)
	read("mpims.json", &mpims)
	for _, c := range channels {
		c["is_channel"] = true
		add("public_channel", c["name"].(string), c)
	}
	for _, c := range groups {
		c["is_channel"], c["is_private"] = true, true
		add("private_channel", c["name"].(string), c)
	}
	for _, c := range mpims {
		c["is_mpim"], c["is_private"] = true, true
		add("mpim", c["name"].(string), c)
	}
	for _, c := range dms {
		other := ""
		for _, m := range c["members"].([]any) {
			if m != self {
				other = m.(string)
			}
		}
		add("im", c["id"].(string), map[string]any{"id": c["id"], "created": c["created"], "is_im": true, "user": other})
	}
	return s
}

// attach gives the message at ts a file.
func (s *slackFromExport) attach(ts string, file map[string]any) {
	m := s.byTs[ts]
	files, _ := m["files"].([]any)
	m["files"] = append(files, file)
}

func (s *slackFromExport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	signedIn := r.Header.Get("Authorization") == "Bearer "+s.token
	if body, ok := s.files[r.URL.Path]; ok {
		if !signedIn {
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<html>Sign in to Slack</html>")
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write(body)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	_ = r.ParseForm()
	s.mu.Lock()
	s.asked = append(s.asked, method)
	s.mu.Unlock()
	answer := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if !signedIn {
		answer(map[string]any{"ok": false, "error": "invalid_auth"})
		return
	}
	page := func(key string, all []any) {
		start, _ := strconv.Atoi(r.Form.Get("cursor"))
		end := min(start+2, len(all))
		next := ""
		if end < len(all) {
			next = strconv.Itoa(end)
		}
		items := all[start:end]
		if items == nil {
			items = []any{}
		}
		answer(map[string]any{"ok": true, key: items, "response_metadata": map[string]any{"next_cursor": next}})
	}
	ch := r.Form.Get("channel")
	switch method {
	case "auth.test":
		answer(map[string]any{"ok": true, "user_id": s.self})
	case "users.list":
		page("members", s.users)
	case "conversations.list":
		page("channels", s.kinds[r.Form.Get("types")])
	case "conversations.members":
		page("members", s.members[ch])
	case "pins.list":
		answer(map[string]any{"ok": true, "items": s.pins[ch]})
	case "conversations.history":
		page("messages", s.history[ch])
	case "conversations.replies":
		page("messages", s.replies[r.Form.Get("ts")])
	default:
		answer(map[string]any{"ok": false, "error": "unknown_method"})
	}
}

func TestASlackWorkspaceReadThroughTheAPIComesAcross(t *testing.T) {
	const token = "xoxp-acme-ada-0123456789"
	w := newAcmeWorld(t, nil)
	slack := newSlackFromExport(t, acmeDir, token, "U0ADA0001")
	srv := httptest.NewServer(slack)
	t.Cleanup(srv.Close)
	was := slackapi.Base
	slackapi.Base = srv.URL + "/api/"
	t.Cleanup(func() { slackapi.Base = was })

	// Ada's house rules carry a file only the token opens; Standup carries one
	// shared from Google Drive, which stays there.
	rules := []byte("1. Be kind.\n2. Ship on Fridays.\n")
	slack.files["/files-pri/T0ACME-F0RULES01/house-rules.txt"] = rules
	slack.attach("1759309260.000200", map[string]any{"id": "F0RULES01", "name": "house-rules.txt", "title": "House rules", "mimetype": "text/plain",
		"size": len(rules), "mode": "hosted", "url_private": srv.URL + "/files-pri/T0ACME-F0RULES01/house-rules.txt",
		"url_private_download": srv.URL + "/files-pri/T0ACME-F0RULES01/house-rules.txt?download=1"})
	slack.attach("1759309680.000600", map[string]any{"id": "F0GDOC001", "name": "Q4 plan", "mode": "external", "is_external": true,
		"external_type": "gdrive", "url_private": "https://docs.google.com/document/d/q4-plan"})

	call := func(h http.HandlerFunc, params map[string]string, body string) (int, map[string]any) {
		t.Helper()
		rc := chi.NewRouteContext()
		for k, v := range params {
			rc.URLParams.Add(k, v)
		}
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(context.WithValue(w.asAdmin(), chi.RouteCtxKey, rc)))
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// The read, asked for through the route, runs on after it answers.
	code, out := call(slackImportController.HandleAPIRead, nil, `{"slack_workspace_name": "Acme", "token": "`+token+`"}`)
	if code != http.StatusAccepted {
		t.Fatalf("the read: %d %v", code, out)
	}
	job, err := uuid.Parse(fmt.Sprint(out["job_id"]))
	if err != nil {
		t.Fatalf("the read's job: %v", out)
	}
	integration.Settle(t)

	var status, stage, source, key, progress string
	if err := w.e.PG.QueryRow(`SELECT status, COALESCE(stage, ''), source, COALESCE(raw_object_key, ''), progress::text FROM import_jobs WHERE id = $1`, job).
		Scan(&status, &stage, &source, &key, &progress); err != nil {
		t.Fatal(err)
	}
	if status != "validating" || stage != "validating" || source != "api" || key != "slackImport/"+job.String()+"/raw.zip" {
		t.Fatalf("after the read the job is %s/%s from %s at %q (progress %s)", status, stage, source, key, progress)
	}
	if _, ok := w.e.store.Written(key); !ok {
		t.Fatalf("no export staged at %s: %v", key, w.e.store.Keys())
	}
	var read struct {
		Read struct {
			Users, Channels, Groups, DMs, MPIMs, Conversations, Of, Messages, Replies, Files int
		} `json:"read"`
	}
	_ = json.Unmarshal([]byte(progress), &read)
	if r := read.Read; r.Users != 4 || r.Channels != 3 || r.Groups != 1 || r.DMs != 1 || r.MPIMs != 1 || r.Conversations != 6 || r.Of != 6 || r.Files != 1 || r.Replies != 3 {
		t.Errorf("what the read found: %+v (%s)", r, progress)
	}
	readNotes := codes(t, w.e, job)
	if readNotes["API_FILE_NOT_READ"] != 1 || len(readNotes) != 1 {
		t.Errorf("the read's notes: %v, want the Drive file's", readNotes)
	}

	// Planned and run through the routes, as an upload's export is.
	if code, out := call(slackImportController.HandlePlan, map[string]string{"jobId": job.String()}, `{}`); code != http.StatusOK {
		t.Fatalf("the plan: %d %v", code, out)
	}
	start := time.Now()
	if code, out := call(slackImportController.HandleRun, map[string]string{"jobId": job.String()}, `{"options": {"skip_subtypes": false}}`); code != http.StatusAccepted {
		t.Fatalf("the run: %d %v", code, out)
	}
	integration.Settle(t)
	notes := w.cameAcross(t, job, time.Since(start))

	// The file the token opened, from the export, in the store as the
	// admin's upload; the Drive file noted, and no link fetched.
	var objKey string
	if err := w.e.PG.QueryRow(`SELECT obj_key FROM attachments WHERE obj_key LIKE '%house-rules.txt'`).Scan(&objKey); err != nil {
		t.Errorf("the file's attachment: %v", err)
	} else if body, ok := w.e.store.Written("userFileUpload/" + objKey); !ok || string(body) != string(rules) {
		t.Errorf("the file in the store at %s: %q (%v)", objKey, body, w.e.store.Keys())
	}
	if notes["FILE_NOT_READ"] != 1 || notes["FILE_GONE"] != 0 || notes["FILE_BLOCKED_URL"] != 0 {
		t.Errorf("the run's file notes: %v, want the Drive file's FILE_NOT_READ alone", notes)
	}

	// Each page asked once, two items a page: the people in two pages, each
	// kind of conversation listed (the channels in two pages), each
	// conversation's people and history page by page, the one thread's four
	// messages in two pages, and the four channels' pins.
	slack.mu.Lock()
	asked := map[string]int{}
	for _, m := range slack.asked {
		asked[m]++
	}
	slack.mu.Unlock()
	want := map[string]int{"auth.test": 1, "users.list": 2, "conversations.list": 5, "conversations.members": 7,
		"pins.list": 4, "conversations.history": 9, "conversations.replies": 2}
	if fmt.Sprint(asked) != fmt.Sprint(want) {
		t.Errorf("what Slack was asked: %v, want %v", asked, want)
	}

	// The token is kept nowhere: not on the job, not in its notes.
	var kept int
	if err := w.e.PG.QueryRow(`SELECT count(*) FROM import_jobs WHERE id = $1 AND (row_to_json(import_jobs)::text LIKE '%' || $2 || '%')`, job, token).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("the token is on the job: %d %v", kept, err)
	}
	if err := w.e.PG.QueryRow(`SELECT count(*) FROM import_errors WHERE import_id = $1 AND row_to_json(import_errors)::text LIKE '%' || $2 || '%'`, job, token).Scan(&kept); err != nil || kept != 0 {
		t.Errorf("the token is in the notes: %d %v", kept, err)
	}
}

// codes is what the job noted, by code.
func codes(t *testing.T, e *raceEnv, job uuid.UUID) map[string]int {
	t.Helper()
	rows, err := e.PG.Query(`SELECT COALESCE(code, '') FROM import_errors WHERE import_id = $1`, job)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		out[c]++
	}
	return out
}
