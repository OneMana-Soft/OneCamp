package business

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	slackapi "github.com/akashc777/OneCamp/services/Slack"
)

// fakeSlack answers the reader as Slack's Web API would, from a workspace
// held in memory, a page at a time.
type fakeSlack struct {
	self      string
	users     []map[string]any
	kinds     map[string][]map[string]any // conversations.list by type
	refuse    map[string]*slackapi.Error  // "method channel-or-type": its refusal
	members   map[string][]string
	pins      map[string][]map[string]any
	history   map[string][]map[string]any // newest first, as Slack answers
	replies   map[string][]map[string]any // by thread ts, root first
	files     map[string]fakeFile         // by link
	page      int                         // items a page (2 when zero)
	calls     []string
	downloads []string
}

type fakeFile struct {
	body string
	kind string
}

func (f *fakeSlack) refusal(method, key string) error {
	if e := f.refuse[method+" "+key]; e != nil {
		return e
	}
	return nil
}

// Call answers method, paging lists by an index cursor.
func (f *fakeSlack) Call(ctx context.Context, method string, form url.Values, out slackapi.Reply) error {
	f.calls = append(f.calls, method+" "+form.Encode())
	size := f.page
	if size == 0 {
		size = 2
	}
	pageOf := func(key string, all []map[string]any) map[string]any {
		start := 0
		if c := form.Get("cursor"); c != "" {
			fmt.Sscan(c, &start)
		}
		end := min(start+size, len(all))
		next := ""
		if end < len(all) {
			next = fmt.Sprint(end)
		}
		return map[string]any{"ok": true, key: all[start:end], "response_metadata": map[string]any{"next_cursor": next}}
	}
	var reply any
	switch method {
	case "auth.test":
		reply = map[string]any{"ok": true, "user_id": f.self}
	case "users.list":
		reply = pageOf("members", f.users)
	case "conversations.list":
		if err := f.refusal(method, form.Get("types")); err != nil {
			return err
		}
		reply = pageOf("channels", f.kinds[form.Get("types")])
	case "conversations.members":
		var all []map[string]any
		for _, m := range f.members[form.Get("channel")] {
			all = append(all, map[string]any{"id": m})
		}
		p := pageOf("members", all)
		var ids []string
		for _, m := range p["members"].([]map[string]any) {
			ids = append(ids, m["id"].(string))
		}
		p["members"] = ids
		reply = p
	case "pins.list":
		if err := f.refusal(method, form.Get("channel")); err != nil {
			return err
		}
		reply = map[string]any{"ok": true, "items": f.pins[form.Get("channel")]}
	case "conversations.history":
		if err := f.refusal(method, form.Get("channel")); err != nil {
			return err
		}
		var since float64
		fmt.Sscan(form.Get("oldest"), &since)
		var all []map[string]any
		for _, m := range f.history[form.Get("channel")] {
			var ts float64
			fmt.Sscan(m["ts"].(string), &ts)
			if ts >= since {
				all = append(all, m)
			}
		}
		reply = pageOf("messages", all)
	case "conversations.replies":
		reply = pageOf("messages", f.replies[form.Get("ts")])
	default:
		return &slackapi.Error{Method: method, Code: "unknown_method"}
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (f *fakeSlack) Download(ctx context.Context, link string, w io.Writer, max int64) (int64, string, error) {
	f.downloads = append(f.downloads, link)
	file, ok := f.files[link]
	if !ok {
		return 0, "", slackapi.ErrFileGone
	}
	n, err := io.WriteString(w, file.body)
	if int64(n) > max {
		return int64(n), "", slackapi.ErrFileTooLarge
	}
	return int64(n), file.kind, err
}

// ts is a Slack ts on day (of October 2025) at hour, with a sequence.
func ts(day, hour, seq int) string {
	return fmt.Sprintf("%d.%06d", time.Date(2025, 10, day, hour, 0, 0, 0, time.UTC).Unix(), seq)
}

// acmeAPI is a small workspace: a channel with a thread, a reply also sent to
// the channel, files of every kind and a pin; a private channel the token
// isn't in; a DM; and a group DM.
func acmeAPI() *fakeSlack {
	root, reply, broadcast := ts(2, 9, 1), ts(2, 10, 1), ts(3, 8, 1)
	file := func(id, name, mime, link string, size int, extra map[string]any) map[string]any {
		f := map[string]any{"id": id, "name": name, "mimetype": mime, "size": size, "url_private_download": link, "url_private": link}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	return &fakeSlack{
		self: "U1",
		users: []map[string]any{
			{"id": "U1", "name": "ada"}, {"id": "U2", "name": "bo"}, {"id": "U3", "name": "cy", "deleted": true},
		},
		kinds: map[string][]map[string]any{
			"public_channel":  {{"id": "C1", "name": "general", "is_channel": true, "created": 1}},
			"private_channel": {{"id": "G1", "name": "leads", "is_group": true, "is_private": true, "created": 1}},
			"mpim":            {{"id": "M1", "name": "mpdm-ada--bo--cy-1", "is_mpim": true, "is_private": true, "created": 1}},
			"im":              {{"id": "D1", "is_im": true, "user": "U2", "created": 1}},
		},
		refuse:  map[string]*slackapi.Error{"conversations.history G1": {Method: "conversations.history", Code: "not_in_channel"}},
		members: map[string][]string{"C1": {"U1", "U2", "U3"}, "G1": {"U2"}, "M1": {"U1", "U2", "U3"}},
		pins:    map[string][]map[string]any{"C1": {{"type": "message", "created": 1759395660, "created_by": "U2", "message": map[string]any{"ts": root}}}},
		history: map[string][]map[string]any{
			"C1": {
				{"ts": broadcast, "user": "U1", "text": "also sent to the channel", "subtype": "thread_broadcast", "thread_ts": root},
				{"ts": root, "user": "U2", "text": "a thread", "thread_ts": root, "reply_count": 2},
				{"ts": ts(1, 11, 1), "user": "U1", "text": "files", "files": []map[string]any{
					file("F1", "notes.txt", "text/plain", "https://files.slack.com/F1", 5, nil),
					file("F2", "plan", "application/vnd.google-apps.document", "https://docs.google.com/d/1", 0, map[string]any{"is_external": true, "mode": "external"}),
					file("F3", "film.mov", "video/quicktime", "https://files.slack.com/F3", 5<<20, nil),
					file("F4", "shot.png", "image/png", "https://files.slack.com/F4", 9, nil),
				}},
				{"ts": ts(1, 9, 1), "user": "U2", "text": "first"},
			},
			"D1": {{"ts": ts(1, 12, 1), "user": "U2", "text": "a DM", "files": []map[string]any{file("F1", "notes.txt", "text/plain", "https://files.slack.com/F1", 5, nil)}}},
			"M1": {{"ts": ts(1, 13, 1), "user": "U3", "text": "a group DM"}},
		},
		replies: map[string][]map[string]any{root: {
			{"ts": root, "user": "U2", "text": "a thread", "thread_ts": root, "reply_count": 2},
			{"ts": reply, "user": "U1", "text": "a reply", "thread_ts": root},
			{"ts": broadcast, "user": "U1", "text": "also sent to the channel", "subtype": "thread_broadcast", "thread_ts": root},
		}},
		files: map[string]fakeFile{
			"https://files.slack.com/F1": {body: "hello", kind: "text/plain"},
			"https://files.slack.com/F4": {body: "<html>sign in</html>", kind: "text/html; charset=utf-8"},
		},
	}
}

// readZip is the zip's entries, by name.
func readZip(t *testing.T, b []byte) (map[string]string, *zip.Reader) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[f.Name] = string(body)
	}
	return out, zr
}

// texts is a day file's messages' texts, in order.
func texts(t *testing.T, day string) []string {
	t.Helper()
	var msgs []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(day), &msgs); err != nil {
		t.Fatalf("%s: %v", day, err)
	}
	var out []string
	for _, m := range msgs {
		out = append(out, m.Text)
	}
	return out
}

// A workspace read through the API is written as Slack writes an export, and
// the importer's parser reads it as one: every conversation the token can
// read, a day a file with each day's replies in it, a reply sent to the
// channel too once, the DM's two people, the channel's pin, and the files the
// token can open under __uploads. What it can't read is noted, not fatal.
func TestAWorkspaceReadThroughTheAPIIsWrittenAsAnExport(t *testing.T) {
	slack := acmeAPI()
	var out bytes.Buffer
	var progress []int
	report, err := ReadAPIExport(context.Background(), slack, APIReadOptions{
		MaxFileBytes: 1 << 20,
		Progress: func(ctx context.Context, r APIReadReport) error {
			progress = append(progress, r.Conversations)
			return nil
		},
	}, &out)
	if err != nil {
		t.Fatalf("the read: %v (calls %q)", err, slack.calls)
	}
	entries, zr := readZip(t, out.Bytes())
	var names []string
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{
		"D1/2025-10-01.json",
		"__uploads/F1/notes.txt",
		"channels.json", "dms.json",
		"general/2025-10-01.json", "general/2025-10-02.json", "general/2025-10-03.json",
		"groups.json",
		"mpdm-ada--bo--cy-1/2025-10-01.json",
		"mpims.json", "users.json",
	}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("the export's files:\n%q\nwant\n%q", names, want)
	}
	for day, want := range map[string][]string{
		"general/2025-10-01.json": {"first", "files"},
		"general/2025-10-02.json": {"a thread", "a reply"},
		"general/2025-10-03.json": {"also sent to the channel"},
	} {
		if got := texts(t, entries[day]); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %q, want %q", day, got, want)
		}
	}
	if entries["__uploads/F1/notes.txt"] != "hello" {
		t.Errorf("the file: %q", entries["__uploads/F1/notes.txt"])
	}
	if strings.Count(strings.Join(slack.downloads, " "), "F1") != 1 || strings.Contains(strings.Join(slack.downloads, " "), "google") {
		t.Errorf("downloads %q: F1 once, and nothing from another service", slack.downloads)
	}

	var dms []struct {
		ID      string   `json:"id"`
		Members []string `json:"members"`
	}
	_ = json.Unmarshal([]byte(entries["dms.json"]), &dms)
	if len(dms) != 1 || fmt.Sprint(dms[0].Members) != "[U1 U2]" {
		t.Errorf("dms.json: %s", entries["dms.json"])
	}
	var channels []struct {
		ID      string           `json:"id"`
		Members []string         `json:"members"`
		Pins    []map[string]any `json:"pins"`
	}
	_ = json.Unmarshal([]byte(entries["channels.json"]), &channels)
	if len(channels) != 1 || len(channels[0].Members) != 3 || len(channels[0].Pins) != 1 || channels[0].Pins[0]["user"] != "U2" {
		t.Errorf("channels.json: %s", entries["channels.json"])
	}
	if strings.TrimSpace(entries["groups.json"]) != "[]" {
		t.Errorf("groups.json holds a channel the token couldn't read: %s", entries["groups.json"])
	}

	// The importer reads it as an export.
	parsed, err := ParseManifests(zr)
	if err != nil {
		t.Fatalf("the parser: %v", err)
	}
	if len(parsed.MessageFiles["C1"]) != 3 || len(parsed.MessageFiles["D1"]) != 1 || len(parsed.MessageFiles["M1"]) != 1 || parsed.Uploads["F1"] == nil || len(parsed.Users) != 3 {
		t.Errorf("parsed: files %v, uploads %v, users %d", parsed.MessageFiles, parsed.Uploads, len(parsed.Users))
	}

	var notes []string
	for _, n := range report.Notes {
		notes = append(notes, n.Code+" "+n.ID+" "+n.Reason)
	}
	sort.Strings(notes)
	wantNotes := []string{
		"API_CONVERSATION_NOT_READ G1 not_in_channel",
		"API_FILE_NOT_READ F2 external",
		"API_FILE_NOT_READ F3 too_large",
		"API_FILE_NOT_READ F4 unreadable",
	}
	if fmt.Sprint(notes) != fmt.Sprint(wantNotes) {
		t.Errorf("notes %q, want %q", notes, wantNotes)
	}
	if report.Users != 3 || report.Channels != 1 || report.Groups != 0 || report.DMs != 1 || report.MPIMs != 1 ||
		report.Messages != 6 || report.Replies != 2 || report.Files != 1 || report.Of != 4 || report.Conversations != 4 {
		t.Errorf("report %+v", report)
	}
	if fmt.Sprint(progress) != "[1 2 3 4]" {
		t.Errorf("progress after each conversation: %v", progress)
	}
}

// A token without the scope for one kind of conversation reads the rest, and
// says which kind it left out and the scope it needed.
func TestAReadWithoutAScopeReadsTheRest(t *testing.T) {
	slack := acmeAPI()
	slack.refuse["conversations.list im"] = &slackapi.Error{Method: "conversations.list", Code: "missing_scope", Needed: "im:read"}
	var out bytes.Buffer
	report, err := ReadAPIExport(context.Background(), slack, APIReadOptions{}, &out)
	if err != nil {
		t.Fatal(err)
	}
	if report.DMs != 0 || report.Channels != 1 || report.MPIMs != 1 {
		t.Errorf("report %+v", report)
	}
	found := false
	for _, n := range report.Notes {
		if n.Code == APINoteKind && n.ID == "im" && n.Reason == "missing_scope im:read" {
			found = true
		}
	}
	if !found {
		t.Errorf("notes %+v, want the DMs left out for im:read", report.Notes)
	}
	if got := apiNoteWords(APIReadNote{Code: APINoteKind, ID: "im", Reason: "missing_scope im:read"}); got != "Slack didn't give the token its DM conversations (missing_scope im:read)." {
		t.Errorf("the note's words: %q", got)
	}
}

// A refused token, or one without the scope to list the people, fails the
// read; Since and MaxMessages bound what is read.
func TestAReadIsBoundedAndFailsOnARefusedToken(t *testing.T) {
	slack := acmeAPI()
	slack.refuse = map[string]*slackapi.Error{}
	var out bytes.Buffer
	since := time.Date(2025, 10, 2, 0, 0, 0, 0, time.UTC)
	report, err := ReadAPIExport(context.Background(), slack, APIReadOptions{Since: since, MaxMessages: 1}, &out)
	if err != nil {
		t.Fatal(err)
	}
	asked := false
	for _, c := range slack.calls {
		if strings.HasPrefix(c, "conversations.history") && strings.Contains(c, "oldest="+fmt.Sprint(since.Unix())) {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the history wasn't asked from Since: %q", slack.calls)
	}
	entries, _ := readZip(t, out.Bytes())
	if got := texts(t, entries["general/2025-10-03.json"]); fmt.Sprint(got) != "[also sent to the channel]" {
		t.Errorf("one message, the newest: %q", got)
	}
	if _, ok := entries["general/2025-10-01.json"]; ok || report.Messages != 1 {
		t.Errorf("read past MaxMessages or Since: %d messages, %v", report.Messages, entries)
	}

	failing := &failingAuth{fakeSlack: acmeAPI()}
	if _, err := ReadAPIExport(context.Background(), failing, APIReadOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "invalid_auth") {
		t.Errorf("a refused token: %v", err)
	}
	if got := apiReadFailure(fmt.Errorf("Slack didn't accept the token: %w", &slackapi.Error{Code: "invalid_auth"}), 0); got != "Slack didn't accept the token (invalid_auth). Check it, and start the read again." {
		t.Errorf("the failure's words: %q", got)
	}
}

type failingAuth struct{ *fakeSlack }

func (f *failingAuth) Call(ctx context.Context, method string, form url.Values, out slackapi.Reply) error {
	if method == "auth.test" {
		return &slackapi.Error{Method: method, Code: "invalid_auth"}
	}
	return f.fakeSlack.Call(ctx, method, form, out)
}

// History over the server's limit fails the read, saying so; a file that
// would take it over is left out and noted; a discarded job's stop stops it.
func TestAReadKeepsToTheLimitAndStops(t *testing.T) {
	_, err := ReadAPIExport(context.Background(), acmeAPI(), APIReadOptions{MaxBytes: 64}, io.Discard)
	if !errors.Is(err, ErrAPIReadTooLarge) {
		t.Fatalf("history over the limit: %v", err)
	}
	if got := apiReadFailure(err, 5<<30); got != "This workspace's history is larger than this server takes (5 GB). Whoever runs it can raise the limit (EXPORT_MAX_BYTES)." {
		t.Errorf("its words: %q", got)
	}

	stop := errors.New("discarded")
	slack := acmeAPI()
	_, err = ReadAPIExport(context.Background(), slack, APIReadOptions{Progress: func(context.Context, APIReadReport) error { return stop }}, io.Discard)
	if !errors.Is(err, stop) {
		t.Fatalf("a stop: %v", err)
	}
	for _, c := range slack.calls {
		if strings.HasPrefix(c, "conversations.history") && !strings.Contains(c, "channel=C1") {
			t.Errorf("read on after the stop: %s", c)
		}
	}
}

// A file's name is one path element in the zip, whatever Slack holds.
func TestAnUploadsNameIsOnePathElement(t *testing.T) {
	for name, want := range map[string]string{
		"notes.txt":         "notes.txt",
		"../../etc/passwd":  "passwd",
		`a\b.txt`:           "a_b.txt",
		"":                  "F9",
		"..":                "F9",
		"report final.pdf ": "report final.pdf",
	} {
		if got := uploadName(apiFile{ID: "F9", Name: name}); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
	if !signInPage("text/html; charset=utf-8", "image/png") || signInPage("text/html", "text/html") || signInPage("image/png", "image/png") {
		t.Error("the sign-in page check")
	}
	if id, ok := uploadID("__uploads/F1"); !ok || id != "F1" {
		t.Errorf("uploadID: %q %v", id, ok)
	}
	for _, dir := range []string{"__uploads", "__uploads/", "__uploads/F1/x", "general"} {
		if _, ok := uploadID(dir); ok {
			t.Errorf("uploadID(%q) took it", dir)
		}
	}
}
