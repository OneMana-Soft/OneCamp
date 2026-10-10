package helpers

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	testVisitor = "visitor@demo.example"
	testOwner   = "owner@example.com"
)

func TestBlankEmailsLeavesOnlyTheVisitorsOwn(t *testing.T) {
	in := `{"data":[{"user_email_id": "owner@example.com","user_name":"Owner"},` +
		`{"user_email_id":"visitor@demo.example"},{"to_email":"x.y+z@co.uk"},` +
		`{"owner@example.com":"a key stays"},{"msg":"write to owner@example.com"},` +
		`{"meta":"{\"email\":\"owner@example.com\",\"me\":\"visitor@demo.example\",\"owner@example.com\": 1}"},` +
		`{"twice":"{\"inner\":\"{\\\"email\\\":\\\"owner@example.com\\\"}\"}"},` +
		`{"n":"@sam"},{"path":"C:\\files\\owner@example.com"}]}`
	got := string(BlankEmails([]byte(in), "Visitor@Demo.Example"))
	want := `{"data":[{"user_email_id": "","user_name":"Owner"},` +
		`{"user_email_id":"visitor@demo.example"},{"to_email":""},` +
		`{"owner@example.com":"a key stays"},{"msg":"write to owner@example.com"},` +
		`{"meta":"{\"email\":\"\",\"me\":\"visitor@demo.example\",\"owner@example.com\": 1}"},` +
		`{"twice":"{\"inner\":\"{\\\"email\\\":\\\"\\\"}\"}"},` +
		`{"n":"@sam"},{"path":"C:\\files\\owner@example.com"}]}`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if !json.Valid([]byte(got)) {
		t.Error("no longer JSON")
	}
	plain := []byte(`{"a":"no addresses here"}`)
	if out := BlankEmails(plain, ""); &out[0] != &plain[0] {
		t.Error("a body with nothing to blank was copied")
	}
}

// A search's highlight puts <mark> tags around what it matched, and WriteJSON
// sends them as \u003cmark\u003e: an address highlighted was no longer exactly
// an address, so it went out to the visitor.
func TestBlankEmailsHidesAHighlightedAddress(t *testing.T) {
	highlight := map[string][]string{
		"user_email": {"<mark>owner@example.com</mark>"},
		"user_name":  {"<mark>owner</mark>@<mark>gmail.com</mark>", "<mark>visitor@demo.example</mark>"},
		"chat_body":  {"write to <mark>owner@example.com</mark> today"},
		"ch_name":    {"<em>owner@example.com</em>"},
	}
	want := map[string][]string{
		"user_email": {""},
		"user_name":  {"", "<mark>visitor@demo.example</mark>"},
		"chat_body":  {"write to <mark>owner@example.com</mark> today"},
		"ch_name":    {"<em>owner@example.com</em>"},
	}
	escaped, err := json.MarshalIndent(Envolope{"highlight": highlight}, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(escaped, []byte(`\u003cmark\u003e`)) {
		t.Fatalf("WriteJSON's encoding no longer escapes the tags, so this misses what it sends: %s", escaped)
	}
	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Envolope{"highlight": highlight}); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"escaped": escaped, "as they are": raw.Bytes()} {
		var got struct {
			Highlight map[string][]string `json:"highlight"`
		}
		if err := json.Unmarshal(BlankEmails(body, testVisitor), &got); err != nil {
			t.Fatalf("%s: no longer JSON: %v", name, err)
		}
		for field, w := range want {
			if fmt.Sprint(got.Highlight[field]) != fmt.Sprint(w) {
				t.Errorf("%s, %s: got %q, want %q", name, field, got.Highlight[field], w)
			}
		}
	}
}

// isEmail is the address pattern, written out by hand because the regexp was
// half the cost of a large answer.
func TestIsEmailIsTheAddressPattern(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z0-9._%+'\-]+@[A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,}$`)
	check := func(b []byte) {
		t.Helper()
		if got, want := isEmail(b), pattern.Match(b); got != want {
			t.Fatalf("%q: isEmail says %v, the pattern %v", b, got, want)
		}
	}
	for _, s := range []string{"owner@example.com", "x.y+z@co.uk", "o'neil@x.org", "a@b", "a@b.c", "@x.com",
		"a@.com", "a@x..com", "a@x.com.", "a@-.io", "a@x.c0m", "a@@x.com", "a b@x.com", "a@x.com\\"} {
		check([]byte(s))
	}
	// Near-addresses, built and then broken at random.
	r := rand.New(rand.NewPCG(1, 2))
	parts := []string{"a", "Z", "9", ".", "-", "_", "%", "+", "'", "@", "..", "co", "uk", "x1", " ", "\\", "\"", "\xc3\xa9"}
	for range 200000 {
		var b []byte
		for range 1 + r.IntN(8) {
			b = append(b, parts[r.IntN(len(parts))]...)
		}
		if r.IntN(2) == 0 {
			b = append(b, []byte("@host."+[]string{"io", "c", "com", "1a", ""}[r.IntN(5)])...)
		}
		check(b)
	}
}

// pagePost is the shape of a channel page's post, near enough.
type pagePost struct {
	UUID      string            `json:"post_uuid"`
	Text      string            `json:"post_text"`
	CreatedAt time.Time         `json:"post_created_at"`
	By        pageUser          `json:"post_by"`
	Reactions []pageReaction    `json:"post_reactions"`
	Meta      string            `json:"post_meta"`
	Extra     map[string]string `json:"extra"`
}

type pageUser struct {
	UUID    string `json:"user_uuid"`
	Name    string `json:"user_name"`
	Full    string `json:"user_full_name"`
	Email   string `json:"user_email_id"`
	Profile string `json:"user_profile_object_key"`
}

type pageReaction struct {
	Emoji string   `json:"reaction_emoji_id"`
	By    pageUser `json:"reaction_added_by"`
}

// channelPage is a channel page of n posts by everyone in turn, the visitor
// among them, as WriteJSON sends it.
func channelPage(t testing.TB, n int) []byte {
	people := []pageUser{
		{"u-1", "owner", "Owner Person", testOwner, "profile/1.png"},
		{"u-2", "sam", "Sam Rivera", testVisitor, "profile/2.png"},
		{"u-3", "member", "Member Person", "member.personal@example.org", ""},
	}
	posts := make([]pagePost, n)
	for i := range posts {
		by := people[i%len(people)]
		posts[i] = pagePost{
			UUID: fmt.Sprintf("post-%d", i),
			Text: strings.Repeat(`<p>Shipping the "launch" notes; see \docs and write to me@ any time.</p>`, 4),
			By:   by, CreatedAt: time.Date(2026, 10, 10, 9, 0, i, 0, time.UTC),
			Reactions: []pageReaction{{"+1", people[(i+1)%3]}, {"tada", people[(i+2)%3]}},
			Meta:      fmt.Sprintf(`{"mentioned":{"email":%q}}`, people[(i+1)%3].Email),
			Extra:     map[string]string{"note": "nothing here", "contact": by.Email},
		}
	}
	body, err := json.MarshalIndent(Envolope{"msg": "ok", "data": posts}, "", "\t")
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBlankEmailsTheSameWhereverTheBodyIsCut(t *testing.T) {
	body := channelPage(t, 12)
	want := BlankEmails(body, testVisitor)
	if bytes.Contains(want, []byte(testOwner)) || !bytes.Contains(want, []byte(testVisitor)) || !json.Valid(want) {
		t.Fatalf("the whole page came out wrong: %s", want)
	}
	cuts := []int{1, 2, 3, 5, 7, 13, 64, 509, 4096}
	for _, size := range cuts {
		b := emailBlanker{keep: []byte(testVisitor)}
		var got []byte
		for i := 0; i < len(body); i += size {
			end := min(i+size, len(body))
			got = append(got, b.feed(body[i:end])...)
		}
		got = append(got, b.drain()...)
		if !bytes.Equal(got, want) {
			t.Errorf("cut every %d bytes, the page came out differently", size)
		}
	}
}

// serve answers one request to a real server whose handler h is served the way
// the auth middleware serves a person signed in as viewer.
func serve(t *testing.T, viewer string, h http.HandlerFunc) (*http.Response, []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeHidingEmails(h, w, r, viewer)
	}))
	defer srv.Close()
	// Raw bytes: no transparent gzip.
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, got
}

func TestServeHidingEmailsOnlyForTheDemoVisitor(t *testing.T) {
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	body := `{"user_email_id":"owner@example.com"}`
	h := func(w http.ResponseWriter, r *http.Request) { WriteJSON(w, http.StatusOK, json.RawMessage(body)) }

	t.Setenv("DEMO_MODE", "")
	if _, got := serve(t, testVisitor, h); !bytes.Contains(got, []byte(testOwner)) {
		t.Errorf("off the demo the visitor's answer changed: %s", got)
	}
	t.Setenv("DEMO_MODE", "true")
	if _, got := serve(t, testOwner, h); !bytes.Contains(got, []byte(testOwner)) {
		t.Errorf("a demo member's own answer changed: %s", got)
	}
	if _, got := serve(t, testVisitor, h); bytes.Contains(got, []byte(testOwner)) || !json.Valid(got) {
		t.Errorf("the visitor still reads it: %s", got)
	}
}

func TestServeHidingEmailsFixesTheLengthAJSONHandlerSet(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	body := []byte(`{"user_email_id":"owner@example.com","n":1}`)
	for _, explicit := range []bool{false, true} {
		resp, got := serve(t, testVisitor, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", fmt.Sprint(len(body)))
			if explicit {
				w.WriteHeader(http.StatusOK)
			}
			_, _ = w.Write(body)
		})
		if string(got) != `{"user_email_id":"","n":1}` {
			t.Errorf("WriteHeader called: %v: got %s", explicit, got)
		}
		if resp.ContentLength != -1 && resp.ContentLength != int64(len(got)) {
			t.Errorf("WriteHeader called: %v: Content-Length %d for a body of %d", explicit, resp.ContentLength, len(got))
		}
	}
}

func TestServeHidingEmailsPassesOtherBodiesByteForByte(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	file := []byte("\x89PNG\r\n\x1a\n" + `{"user_email_id":"owner@example.com"}` + strings.Repeat("\x00\xff", 40000))
	for _, ct := range []string{"application/octet-stream", "image/png", "text/csv", "text/html; charset=utf-8", "text/plain"} {
		// http.ServeContent writes through io.Copy, so the file takes the
		// ReadFrom path (sendfile, for a real file).
		resp, got := serve(t, testVisitor, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(file))
		})
		if !bytes.Equal(got, file) {
			t.Errorf("%s: the body changed", ct)
		}
		if resp.ContentLength != int64(len(file)) {
			t.Errorf("%s: Content-Length %d, want %d", ct, resp.ContentLength, len(file))
		}
	}

	// A compressed answer can't be read here, so it goes as it is.
	// Stored, not deflated, so the address is there in the bytes to find.
	var zipped bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zipped, gzip.NoCompression)
	_, _ = zw.Write([]byte(`{"user_email_id":"owner@example.com"}`))
	_ = zw.Close()
	_, got := serve(t, testVisitor, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(zipped.Bytes())
	})
	if !bytes.Equal(got, zipped.Bytes()) {
		t.Error("a gzipped answer was rewritten")
	}
}

// copyWatcher is a writer that can take a file whole (io.ReaderFrom, which
// net/http answers with sendfile), and notes when it was asked to.
type copyWatcher struct {
	*httptest.ResponseRecorder
	readFrom bool
}

func (c *copyWatcher) ReadFrom(src io.Reader) (int64, error) {
	c.readFrom = true
	return io.Copy(c.ResponseRecorder, src)
}

func TestServeHidingEmailsHandsAFileToTheWriterUnderneath(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	body := `{"user_email_id":"owner@example.com"}`
	for ct, whole := range map[string]bool{"application/pdf": true, "application/json": false} {
		w := &copyWatcher{ResponseRecorder: httptest.NewRecorder()}
		ServeHidingEmails(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = io.Copy(w, pieces{strings.NewReader(body)})
		}), w, httptest.NewRequest(http.MethodGet, "/", nil), testVisitor)
		if w.readFrom != whole {
			t.Errorf("%s: handed on whole: %v, want %v", ct, w.readFrom, whole)
		}
		if blanked := !strings.Contains(w.Body.String(), testOwner); blanked == whole {
			t.Errorf("%s: got %s", ct, w.Body.String())
		}
	}
}

// pieces reads a few bytes at a time and has no WriteTo, as a stream does.
type pieces struct{ r io.Reader }

func (p pieces) Read(b []byte) (int, error) { return p.r.Read(b[:min(len(b), 7)]) }

func TestServeHidingEmailsReadsJSONCopiedInPieces(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	page := channelPage(t, 30)
	_, got := serve(t, testVisitor, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.Copy(w, pieces{bytes.NewReader(page)}); err != nil {
			t.Error(err)
		}
	})
	if !bytes.Equal(got, BlankEmails(page, testVisitor)) {
		t.Error("JSON copied in pieces came out differently from the same JSON written whole")
	}
}

func TestServeHidingEmailsStreamsEachEventWhenItsHandlerFlushes(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	firstRead := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ServeHidingEmails(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// What the AI's streams write: the same checks, then frames.
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("the visitor's writer is no Flusher")
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: %s\n\n", `{"sources":[{"author_email":"owner@example.com","me":"visitor@demo.example"}]}`)
			flusher.Flush()
			// The next frame waits until the client has the first: a writer
			// that held answers back until the end would never get here.
			select {
			case <-firstRead:
			case <-time.After(5 * time.Second):
				t.Error("the first event never reached the client before the handler went on")
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", `{"content":"done"}`)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("ResponseController can't flush through the writer: %v", err)
			}
		}), w, r, testVisitor)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := bufio.NewReader(resp.Body)
	first, err := lines.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	close(firstRead)
	if first != `data: {"sources":[{"author_email":"","me":"visitor@demo.example"}]}`+"\n" {
		t.Errorf("first event: %q", first)
	}
	rest, _ := io.ReadAll(lines)
	if string(rest) != "\n"+`data: {"content":"done"}`+"\n\n" {
		t.Errorf("rest of the stream: %q", rest)
	}
}

func TestServeHidingEmailsOffersWhatTheWriterUnderneathOffers(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)

	// A real server's writer: Flusher, Hijacker and ReaderFrom, and a
	// hijacked connection is the real one.
	_, got := serve(t, testVisitor, func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("no Flusher")
		}
		if _, ok := w.(io.ReaderFrom); !ok {
			t.Error("no ReaderFrom")
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no Hijacker")
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 15\r\nConnection: close\r\n\r\nraw connection.")
		_ = buf.Flush()
	})
	if string(got) != "raw connection." {
		t.Errorf("over the hijacked connection: %q", got)
	}

	// A writer that can only flush is offered as one that can only flush.
	rec := httptest.NewRecorder()
	ServeHidingEmails(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := w.(http.Hijacker); ok {
			t.Error("claims to hijack over a writer that can't")
		}
		if _, ok := w.(http.Flusher); !ok {
			t.Error("lost the Flusher")
		}
	}), rec, httptest.NewRequest(http.MethodGet, "/", nil), testVisitor)
}

func TestServeHidingEmailsWrapsOnce(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	rec := httptest.NewRecorder()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, Envolope{"user_email_id": testOwner})
	})
	outer := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A second auth middleware on the same route.
		ServeHidingEmails(http.HandlerFunc(func(w2 http.ResponseWriter, r2 *http.Request) {
			if w2 != w {
				t.Error("wrapped twice")
			}
			inner(w2, r2)
		}), w, r, testVisitor)
	})
	ServeHidingEmails(outer, rec, httptest.NewRequest(http.MethodGet, "/", nil), testVisitor)
	if strings.Contains(rec.Body.String(), testOwner) {
		t.Errorf("got %s", rec.Body.String())
	}
}

func TestDemoMqttPayload(t *testing.T) {
	t.Setenv("DEMO_USER_EMAIL", testVisitor)
	p := []byte(`{"u":"owner@example.com","v":"visitor@demo.example"}`)
	t.Setenv("DEMO_MODE", "")
	if got := DemoMqttPayload(p).([]byte); string(got) != string(p) {
		t.Errorf("off the demo: %s", got)
	}
	t.Setenv("DEMO_MODE", "true")
	if got := DemoMqttPayload(p).([]byte); string(got) != `{"u":"","v":"visitor@demo.example"}` {
		t.Errorf("on the demo: %s", got)
	}
	if got := DemoMqttPayload(string(p)).(string); got != `{"u":"","v":"visitor@demo.example"}` {
		t.Errorf("a string payload: %s", got)
	}
}

// BenchmarkBlankEmailsChannelPage is the cost on a large answer: a channel
// page of 200 posts, each with its author, two reactions and a mention.
func BenchmarkBlankEmailsChannelPage(b *testing.B) {
	page := channelPage(b, 200)
	b.SetBytes(int64(len(page)))
	b.ReportAllocs()
	for b.Loop() {
		BlankEmails(page, testVisitor)
	}
}

// BenchmarkMarshalChannelPage is what WriteJSON already spends on the same
// page, to set the cost above against.
func BenchmarkMarshalChannelPage(b *testing.B) {
	page := channelPage(b, 200)
	var v any
	if err := json.Unmarshal(page, &v); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(page)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = json.MarshalIndent(v, "", "\t")
	}
}
