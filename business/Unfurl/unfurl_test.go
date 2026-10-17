package business

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	"github.com/google/uuid"
)

func TestCanonicalStripsTrackersAndKeepsTheRest(t *testing.T) {
	u, err := Canonical(" HTTPS://Example.com/a/b?utm_source=x&id=7&fbclid=y&si=z&UTM_Medium=q#part ")
	if err != nil {
		t.Fatal(err)
	}
	if got := u.String(); got != "https://example.com/a/b?id=7#part" {
		t.Errorf("got %s", got)
	}
	for _, bad := range []string{"", "ftp://x.com/", "javascript:alert(1)", "example.com", "http://", strings.Repeat("a", 3000)} {
		if _, err := Canonical(bad); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("Canonical(%q) = %v; want ErrInvalidURL", bad, err)
		}
	}
	a, _ := Canonical("https://example.com/x?utm_campaign=1#a")
	b, _ := Canonical("https://example.com/x#b")
	if cacheKey(a) != cacheKey(b) {
		t.Error("one page, two fragments and a tracker: one cache entry")
	}
}

func TestKnownSitesAnswerThroughTheirOEmbed(t *testing.T) {
	cases := map[string]string{
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ":                 "youtube",
		"https://youtu.be/dQw4w9WgXcQ":                                "youtube",
		"https://vimeo.com/76979871":                                  "vimeo",
		"https://www.loom.com/share/0281766fa2d04bb788eaf19e65135184": "loom",
		"https://open.spotify.com/track/4uLU6hMCjMI75M1A2tKUQC":       "spotify",
		"https://x.com/jack/status/20":                                "x",
		"https://twitter.com/jack/status/20":                          "x",
		"https://codepen.io/team/pen/abcDEF":                          "codepen",
		"https://www.figma.com/design/AbC123/Plan":                    "figma",
		"https://miro.com/app/board/uXjVabc=/":                        "miro",
	}
	for raw, want := range cases {
		u, _ := url.Parse(raw)
		if p, ok := providerFor(u); !ok || p.id != want {
			t.Errorf("%s: %v %v; want %s", raw, p.id, ok, want)
		}
	}
	for _, raw := range []string{"https://www.youtube.com/", "https://x.com/jack", "https://example.com/watch?v=1"} {
		u, _ := url.Parse(raw)
		if p, ok := providerFor(u); ok {
			t.Errorf("%s matched %s; it isn't a video or post", raw, p.id)
		}
	}
}

const page = `<!doctype html><html><head>
<meta charset="utf-8"><title> Plain   title </title>
<meta property="og:title" content="Launch &amp; learn">
<meta name="description" content="The page's own words">
<meta property="og:description" content="Open Graph words">
<meta property="og:site_name" content="Example">
<meta property="og:image" content="/img/card.png">
<link rel="mask-icon" href="/mask.svg">
<link rel="icon" type="image/svg+xml" href="/icon.svg">
<link rel="apple-touch-icon" href="/touch.png">
<link rel="icon" sizes="32x32" href="/favicon-32.png">
<script>document.title = "never run"</script>
</head><body><p>body</p></body></html>`

func TestAPageSaysWhatItIs(t *testing.T) {
	base, _ := url.Parse("https://example.com/post/1")
	var c Card
	readPage([]byte(page), "text/html; charset=utf-8").fill(&c, base)
	if c.Title != "Launch & learn" || c.Description != "Open Graph words" || c.SiteName != "Example" {
		t.Errorf("got %+v", c)
	}
	if c.ImageURL != "https://example.com/img/card.png" || c.IconURL != "https://example.com/favicon-32.png" {
		t.Errorf("image %s icon %s", c.ImageURL, c.IconURL)
	}
	var bare Card
	readPage([]byte("<title>Only\n a title</title>"), "text/html").fill(&bare, base)
	if bare.Title != "Only a title" || bare.IconURL != "https://example.com/favicon.ico" {
		t.Errorf("got %+v", bare)
	}
}

func TestAPageInAnotherCharacterSetIsRead(t *testing.T) {
	// "Café" in ISO-8859-1.
	body := []byte("<html><head><meta charset=\"iso-8859-1\"><title>Caf\xe9</title></head></html>")
	if got := readPage(body, "text/html").title; got != "Café" {
		t.Errorf("got %q", got)
	}
}

func TestAPostOnXIsItsAuthorAndItsWords(t *testing.T) {
	body := []byte(`{"author_name":"Jack","html":"<blockquote class=\"twitter-tweet\"><p lang=\"en\">just setting up my <a href=\"#\">twttr</a></p>&mdash; jack</blockquote>","provider_name":"Twitter","type":"rich"}`)
	var c Card
	if !readOEmbed(body, providers[5], &c) {
		t.Fatal("not read")
	}
	if c.Title != "Jack on X" || c.Description != "just setting up my twttr" {
		t.Errorf("got %+v", c)
	}
}

func TestCleanCapsAndTidies(t *testing.T) {
	if got := clean("a\x00b \n\t c", 50); got != "a b c" {
		t.Errorf("got %q", got)
	}
	if got := clean(strings.Repeat("é", 10), 5); got != "éééé…" {
		t.Errorf("got %q", got)
	}
}

// fakes swaps the fetch and the cache for the test.
type fakes struct {
	pages   map[string]*safefetch.Response
	errs    map[string]error
	fetched []string
	cache   map[string]Card
	ttls    map[string]time.Duration
}

func useFakes(t *testing.T) *fakes {
	f := &fakes{pages: map[string]*safefetch.Response{}, errs: map[string]error{}, cache: map[string]Card{}, ttls: map[string]time.Duration{}}
	oldFetch, oldGet, oldSet := fetch, getCache, setCache
	fetch = func(_ context.Context, raw string, _ safefetch.Options) (*safefetch.Response, error) {
		f.fetched = append(f.fetched, raw)
		if err := f.errs[raw]; err != nil {
			return nil, err
		}
		if r, ok := f.pages[raw]; ok {
			return r, nil
		}
		return &safefetch.Response{Status: 404, URL: mustURL(raw)}, nil
	}
	getCache = func(_ context.Context, key string, c *Card) bool {
		got, ok := f.cache[key]
		*c = got
		return ok
	}
	setCache = func(_ context.Context, key string, c Card, ttl time.Duration) { f.cache[key], f.ttls[key] = c, ttl }
	t.Cleanup(func() { fetch, getCache, setCache = oldFetch, oldGet, oldSet })
	return f
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func htmlPage(raw, body string) *safefetch.Response {
	return &safefetch.Response{URL: mustURL(raw), Status: 200, ContentType: "text/html", Header: http.Header{"Content-Type": {"text/html"}}, Body: []byte(body)}
}

func TestUnfurlCachesAndAsksTheBudgetOnlyOnAMiss(t *testing.T) {
	f := useFakes(t)
	f.pages["https://example.com/a"] = htmlPage("https://example.com/a", page)
	asked := 0
	budget := func() error { asked++; return nil }
	c, err := Unfurl(context.Background(), "https://example.com/a?utm_source=x", budget)
	if err != nil || !c.Fetched || c.Title != "Launch & learn" || c.URL != "https://example.com/a" {
		t.Fatalf("got %+v %v", c, err)
	}
	if _, err := Unfurl(context.Background(), "https://example.com/a#later", budget); err != nil {
		t.Fatal(err)
	}
	if asked != 1 || len(f.fetched) != 1 {
		t.Errorf("budget asked %d times, %d fetches; the second is the cache's", asked, len(f.fetched))
	}
	for _, ttl := range f.ttls {
		if ttl != 24*time.Hour {
			t.Errorf("a card is cached %v; want a day", ttl)
		}
	}
}

func TestARefusedBudgetFetchesNothing(t *testing.T) {
	f := useFakes(t)
	limited := errors.New("limited")
	if _, err := Unfurl(context.Background(), "https://example.com/b", func() error { return limited }); !errors.Is(err, limited) {
		t.Fatalf("got %v", err)
	}
	if len(f.fetched) != 0 || len(f.cache) != 0 {
		t.Error("a refused budget fetches and caches nothing")
	}
}

func TestAPreviewThatFailsIsQuietAndRememberedBriefly(t *testing.T) {
	f := useFakes(t)
	f.errs["http://intranet.example/x"] = safefetch.ErrBlocked
	f.errs["https://db.corp.example/"] = safefetch.ErrResolvesInside
	f.errs["https://down.example/"] = errors.New("connection refused")
	f.pages["https://example.com/file.pdf"] = &safefetch.Response{URL: mustURL("https://example.com/file.pdf"), Status: 200, ContentType: "application/pdf"}
	f.pages["https://docs.google.com/document/d/1/edit"] = htmlPage("https://accounts.google.com/signin", "<title>Sign in</title>")
	want := map[string]string{
		"http://intranet.example/x": ReasonBlocked,
		// A name the server's DNS answers with an inside address reads as
		// one it can't reach: no oracle for which internal names exist.
		"https://db.corp.example/":                  ReasonUnreachable,
		"https://down.example/":                     ReasonUnreachable,
		"https://example.com/file.pdf":              ReasonNotHTML,
		"https://example.com/missing":               ReasonBadStatus,
		"https://docs.google.com/document/d/1/edit": ReasonSignIn,
	}
	for raw, reason := range want {
		c, err := Unfurl(context.Background(), raw, nil)
		if err != nil || c.Fetched || c.Reason != reason || c.Title != "" {
			t.Errorf("%s: %+v %v; want reason %s", raw, c, err, reason)
		}
	}
	for _, ttl := range f.ttls {
		if ttl != failureTTL {
			t.Errorf("a failure is cached %v; want %v", ttl, failureTTL)
		}
	}
}

func TestAVideoIsReadFromItsOEmbedAndFallsBackToItsPage(t *testing.T) {
	f := useFakes(t)
	link := "https://www.youtube.com/watch?v=abc123"
	f.pages[providers[0].oembedURL(link)] = &safefetch.Response{Status: 200, Body: []byte(`{"title":"A talk","author_name":"Conf","provider_name":"YouTube","thumbnail_url":"https://i.ytimg.com/vi/abc123/hq.jpg","type":"video"}`)}
	c, _ := Unfurl(context.Background(), link, nil)
	if c.Provider != "youtube" || c.Title != "A talk" || c.Author != "Conf" || c.IconURL != "https://www.youtube.com/favicon.ico" || c.Kind != "video" {
		t.Errorf("got %+v", c)
	}
	private := "https://www.youtube.com/watch?v=private1"
	f.pages[private] = htmlPage(private, "<title>Private video</title>")
	c, _ = Unfurl(context.Background(), private, nil)
	if c.Provider != "" || c.Title != "Private video" || !c.Fetched {
		t.Errorf("got %+v", c)
	}
}

func TestStoreRemoteKeepsAPictureForAnEditorOnly(t *testing.T) {
	f := useFakes(t)
	doc := uuid.NewString()
	level := accessBusiness.LevelEdit
	oldAccess, oldUpload, oldLimit := resolveAccess, uploadToDoc, uploadLimit
	var stored []string
	resolveAccess = func(context.Context, accessBusiness.Subject, string) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: level}, nil
	}
	uploadToDoc = func(_ context.Context, body []byte, name, docUUID string) (uuid.UUID, error) {
		stored = append(stored, name+"@"+docUUID)
		return uuid.New(), nil
	}
	uploadLimit = func() int64 { return 10 << 20 }
	t.Cleanup(func() { resolveAccess, uploadToDoc, uploadLimit = oldAccess, oldUpload, oldLimit })

	png := []byte("\x89PNG\r\n\x1a\n" + strings.Repeat("\x00", 32))
	f.pages["https://example.com/pics/Cat%20photo.png"] = &safefetch.Response{URL: mustURL("https://example.com/pics/Cat%20photo.png"), Status: 200, Body: png}
	f.pages["https://example.com/evil.svg"] = &safefetch.Response{URL: mustURL("https://example.com/evil.svg"), Status: 200, Body: []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)}
	f.pages["https://example.com/huge.png"] = &safefetch.Response{URL: mustURL("https://example.com/huge.png"), Status: 200, Body: png, Truncated: true}

	s, err := StoreRemote(context.Background(), accessBusiness.Subject{}, doc, "https://example.com/pics/Cat%20photo.png?utm_source=x", KindImage, nil)
	if err != nil || s.ContentType != "image/png" || s.FileName != "Cat-photo.png" || len(stored) != 1 || stored[0] != "Cat-photo.png@"+doc {
		t.Fatalf("got %+v %v %v", s, err, stored)
	}
	var ns *NotStoredError
	if _, err := StoreRemote(context.Background(), accessBusiness.Subject{}, doc, "https://example.com/evil.svg", KindImage, nil); !errors.As(err, &ns) || ns.Reason != ReasonNotImage {
		t.Errorf("an SVG: %v; want not_image", err)
	}
	if _, err := StoreRemote(context.Background(), accessBusiness.Subject{}, doc, "https://example.com/huge.png", KindImage, nil); !errors.As(err, &ns) || ns.Reason != ReasonTooLarge {
		t.Errorf("a picture over the cap: %v; want too_large", err)
	}
	if _, err := StoreRemote(context.Background(), accessBusiness.Subject{}, doc, "https://example.com/a.png", "script", nil); !errors.Is(err, ErrBadKind) {
		t.Errorf("a bad kind: %v", err)
	}
	level = accessBusiness.LevelComment
	before := len(f.fetched)
	if _, err := StoreRemote(context.Background(), accessBusiness.Subject{}, doc, "https://example.com/pics/Cat%20photo.png", KindImage, nil); !errors.Is(err, ErrCantEdit) {
		t.Errorf("a commenter: %v; want ErrCantEdit", err)
	}
	level = accessBusiness.LevelNone
	if _, err := StoreRemote(context.Background(), accessBusiness.Subject{}, doc, "https://example.com/pics/Cat%20photo.png", KindImage, nil); !errors.Is(err, ErrNoSuchDoc) {
		t.Errorf("no access: %v; want ErrNoSuchDoc", err)
	}
	if len(f.fetched) != before {
		t.Error("nothing is fetched for someone who can't edit the doc")
	}
	if _, err := StoreRemote(context.Background(), accessBusiness.Subject{}, "not-a-uuid", "https://example.com/a.png", KindIcon, nil); !errors.Is(err, ErrNoSuchDoc) {
		t.Errorf("a bad doc id: %v", err)
	}
}

func TestFileNamesAreSafe(t *testing.T) {
	cases := map[[2]string]string{
		{"/a/../../etc/passwd.png", ".png"}: "passwd.png",
		{"/", ".ico"}:                       "icon.ico",
		{"/favicon.ico", ".png"}:            "favicon.png",
		{"/<script>x.gif", ".gif"}:          "script-x.gif",
	}
	for in, want := range cases {
		kind := KindImage
		if in[0] == "/" {
			kind = KindIcon
		}
		if got := fileNameFor(in[0], kind, in[1]); got != want {
			t.Errorf("fileNameFor(%q) = %q; want %q", in[0], got, want)
		}
	}
}
