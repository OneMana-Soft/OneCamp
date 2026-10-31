package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	business "github.com/akashc777/OneCamp/business/Publish"
	viewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
)

const (
	slug  = "launch-notes-1a2b3c4d"
	fileA = "11111111-2222-4333-8444-555555555555"
)

// public is the three public routes as router/docsPublish.go mounts them,
// behind the security headers every route gets.
func public() http.Handler {
	r := chi.NewRouter()
	r.Use(customMiddleware.SecurityHeaders)
	r.Get("/public/p/{slug}", PublishedPage)
	r.Post("/public/p/{slug}/unlock", PublishedPageUnlock)
	r.Get("/public/p/{slug}/state", PublishedPageState)
	r.Get("/public/p/{slug}/files/{obj_uuid}", PublishedFile)
	r.Get("/public/d/{token}", LinkPage)
	r.Post("/public/d/{token}/unlock", LinkPageUnlock)
	r.Get("/public/d/{token}/files/{obj_uuid}", LinkFile)
	return r
}

type seams struct {
	page   func(ctx context.Context, ref business.Ref, token string) (*business.Page, error)
	unlock func(ctx context.Context, ref business.Ref, password, ip string) (string, time.Duration, error)
	file   func(ctx context.Context, ref business.Ref, obj, token string) (*business.File, error)
	stored func(ctx context.Context, key string) (*storedFile, error)
	ips    []string
	allow  bool
	visits []string
}

func stub(t *testing.T) *seams {
	t.Helper()
	t.Setenv("BACKEND_DOMAIN", "https://api.example.test")
	t.Setenv("FRONTEND_DOMAIN", "https://app.example.test")
	t.Setenv("TRUST_PROXY_HEADERS", "")
	s := &seams{allow: true}
	oldP, oldU, oldF, oldS, oldA, oldV := pageFor, unlockPage, fileFor, openStored, addressAllowed, countVisit
	t.Cleanup(func() {
		pageFor, unlockPage, fileFor, openStored, addressAllowed, countVisit = oldP, oldU, oldF, oldS, oldA, oldV
	})
	countVisit = func(_ context.Context, kind, id, scope string, v viewBusiness.Visit) error {
		s.visits = append(s.visits, fmt.Sprintf("%s|%s|%s|%s|gpc=%v|dnt=%v", kind, id, scope, v.Address, v.GPC, v.DNT))
		return nil
	}
	pageFor = func(ctx context.Context, ref business.Ref, token string) (*business.Page, error) {
		return s.page(ctx, ref, token)
	}
	unlockPage = func(ctx context.Context, ref business.Ref, password, ip string) (string, time.Duration, error) {
		return s.unlock(ctx, ref, password, ip)
	}
	fileFor = func(ctx context.Context, ref business.Ref, obj, token string) (*business.File, error) {
		return s.file(ctx, ref, obj, token)
	}
	openStored = func(ctx context.Context, key string) (*storedFile, error) { return s.stored(ctx, key) }
	addressAllowed = func(_ context.Context, surface, ip string, _ int) redisStore.RateLimitResult {
		s.ips = append(s.ips, surface+"|"+ip)
		return redisStore.RateLimitResult{Allowed: s.allow}
	}
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) {
		return &business.Page{Slug: slug, Title: "Launch notes", HTML: "<p>We ship on Friday.</p>",
			UpdatedAt: time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC), DocUUID: "doc-1", Grant: "grant-1"}, nil
	}
	return s
}

func get(h http.Handler, target string, edit ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = "203.0.113.9:40000"
	for _, e := range edit {
		e(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// directives is a policy, directive by directive.
func directives(policy string) map[string]string {
	out := map[string]string{}
	for _, d := range strings.Split(policy, ";") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		name, value, _ := strings.Cut(d, " ")
		out[name] = value
	}
	return out
}

// Condition 1: an opaque origin that runs nothing, posts nowhere, sits in no
// frame, loads only this server's pictures; on every answer the page gives.
func TestThePageRunsNothingAndLoadsNothingElse(t *testing.T) {
	s := stub(t)
	h := public()
	answers := map[string]error{"page": nil, "missing": business.ErrNotFound, "gone": business.ErrGone, "locked": business.ErrLocked,
		"busy": business.ErrBusy, "broken": errors.New("dgraph is down")}
	for name, err := range answers {
		err := err
		s.page = func(context.Context, business.Ref, string) (*business.Page, error) {
			if err != nil {
				return nil, err
			}
			return &business.Page{Slug: slug, Title: "Launch notes", HTML: "<p>Hi</p>"}, nil
		}
		w := get(h, "/public/p/"+slug)
		d := directives(w.Header().Get("Content-Security-Policy"))
		want := map[string]string{
			"sandbox": "allow-downloads", "default-src": "'none'", "img-src": "'self' https://api.example.test",
			"style-src": business.StyleSource, "base-uri": "'none'", "form-action": "'none'", "frame-ancestors": "'none'",
		}
		for k, v := range want {
			if d[k] != v {
				t.Errorf("%s: %s is %q, want %q", name, k, d[k], v)
			}
		}
		if len(d) != len(want) {
			t.Errorf("%s: the policy has more than it should: %v", name, d)
		}
		if strings.Contains(d["sandbox"], "allow-same-origin") || strings.Contains(d["sandbox"], "allow-scripts") {
			t.Errorf("%s: the sandbox lets the page be itself: %q", name, d["sandbox"])
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: headers %v", name, w.Header())
		}
		if w.Header().Get("Content-Type") != "text/html; charset=utf-8" || strings.Contains(strings.ToLower(w.Body.String()), "<script") {
			t.Errorf("%s: not a script-free document", name)
		}
	}
}

// Condition 2: the same bytes for everyone; no cookie read, none set.
func TestThePageIsTheSameForEveryone(t *testing.T) {
	s := stub(t)
	var tokens []string
	s.page = func(_ context.Context, _ business.Ref, token string) (*business.Page, error) {
		tokens = append(tokens, token)
		return &business.Page{Slug: slug, Title: "Launch notes", HTML: "<p>We ship on Friday.</p>"}, nil
	}
	h := public()
	stranger := get(h, "/public/p/"+slug)
	member := get(h, "/public/p/"+slug, func(r *http.Request) {
		r.Header.Set("Cookie", "access_token=eyJhbGciOi.member.sig; refresh_token=r1; csrf_token=c1; demo_visitor=1")
		r.Header.Set("Authorization", "Bearer oc_live_token")
	})
	if !bytes.Equal(stranger.Body.Bytes(), member.Body.Bytes()) || stranger.Code != http.StatusOK || member.Code != http.StatusOK {
		t.Errorf("a member is shown something else:\n%s\n---\n%s", stranger.Body, member.Body)
	}
	for _, w := range []*httptest.ResponseRecorder{stranger, member} {
		if c := w.Result().Header.Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("the page sets a cookie: %v", c)
		}
		if strings.Contains(w.Header().Get("Vary"), "Cookie") {
			t.Error("the page varies by cookie")
		}
	}
	if stranger.Header().Get("ETag") != member.Header().Get("ETag") || len(tokens) != 2 || tokens[0] != "" || tokens[1] != "" {
		t.Errorf("etags %q %q, tokens %v", stranger.Header().Get("ETag"), member.Header().Get("ETag"), tokens)
	}
}

// Condition 3, at the route: another doc's file under this page's address is
// a 404 (the service decides it: business TestAPagesFilesAreOnlyItsDocs, and
// the integration test with real rows).
func TestAnotherDocsFileIsNotThere(t *testing.T) {
	s := stub(t)
	opened := false
	s.file = func(_ context.Context, _ business.Ref, obj, _ string) (*business.File, error) {
		if obj != fileA {
			return nil, business.ErrNotFound
		}
		return &business.File{ObjKey: "u1/" + fileA + "_a.png", ETag: `"` + fileA + `"`}, nil
	}
	s.stored = func(context.Context, string) (*storedFile, error) { opened = true; return nil, errNoStoredFile }
	w := get(public(), "/public/p/"+slug+"/files/66666666-7777-4888-9999-aaaaaaaaaaaa")
	if w.Code != http.StatusNotFound || opened || w.Header().Get("ETag") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("another doc's file: %d %v (opened %v)", w.Code, w.Header(), opened)
	}
}

// Condition 4: the visitor's address is counted, under the trusted-proxy
// rule every public route uses; a forwarded header from anyone else is
// ignored.
func TestTheVisitorIsCountedNotWhatTheyClaim(t *testing.T) {
	s := stub(t)
	s.file = func(context.Context, business.Ref, string, string) (*business.File, error) {
		return nil, business.ErrNotFound
	}
	s.unlock = func(context.Context, business.Ref, string, string) (string, time.Duration, error) {
		return "", 0, business.ErrWrongPassword
	}
	h := public()
	spoof := func(r *http.Request) {
		r.Header.Set("X-Forwarded-For", "198.51.100.7")
		r.Header.Set("X-Real-IP", "198.51.100.8")
		r.Header.Set("CF-Connecting-IP", "198.51.100.9")
	}
	get(h, "/public/p/"+slug, spoof)
	get(h, "/public/p/"+slug+"/files/"+fileA, spoof)
	r := httptest.NewRequest(http.MethodPost, "/public/p/"+slug+"/unlock", strings.NewReader(`{"password":"x"}`))
	r.RemoteAddr = "203.0.113.9:40000"
	spoof(r)
	h.ServeHTTP(httptest.NewRecorder(), r)
	want := []string{"page|203.0.113.9", "page-file|203.0.113.9", "page-unlock|203.0.113.9"}
	if strings.Join(s.ips, ",") != strings.Join(want, ",") {
		t.Errorf("counted %v, want %v", s.ips, want)
	}

	// Behind the proxy (a private peer), the visitor it names; behind
	// Cloudflare too, the one Cloudflare names.
	s.ips = nil
	get(h, "/public/p/"+slug, func(r *http.Request) {
		r.RemoteAddr = "10.0.0.5:40000"
		r.Header.Set("X-Forwarded-For", "198.51.100.7")
	})
	get(h, "/public/p/"+slug, func(r *http.Request) {
		r.RemoteAddr = "10.0.0.5:40000"
		r.Header.Set("X-Forwarded-For", "198.51.100.7, 173.245.48.10")
		r.Header.Set("CF-Connecting-IP", "192.0.2.44")
	})
	if strings.Join(s.ips, ",") != "page|198.51.100.7,page|192.0.2.44" {
		t.Errorf("behind the proxy: %v", s.ips)
	}

	// Over the limit: the calm busy page, told when to come back.
	s.allow = false
	w := get(h, "/public/p/"+slug)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" || !strings.Contains(w.Body.String(), "This page is busy") ||
		w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("over the limit: %d %v", w.Code, w.Header())
	}
}

// Condition 5: a page may be kept only if the cache asks first, so the very
// next ask after an unpublish answers 404, with nothing kept.
func TestUnpublishingAnswersAtOnce(t *testing.T) {
	s := stub(t)
	h := public()
	w := get(h, "/public/p/"+slug)
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-cache" || etag == "" {
		t.Fatalf("the page: %d %v", w.Code, w.Header())
	}
	if again := get(h, "/public/p/"+slug, func(r *http.Request) { r.Header.Set("If-None-Match", etag) }); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("an unchanged page: %d", again.Code)
	}

	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, business.ErrNotFound }
	gone := get(h, "/public/p/"+slug, func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if gone.Code != http.StatusNotFound || gone.Header().Get("Cache-Control") != "no-store" || gone.Header().Get("ETag") != "" {
		t.Errorf("after an unpublish: %d %v", gone.Code, gone.Header())
	}
	if !bytes.Equal(gone.Body.Bytes(), business.MissingDocument().Body) || strings.Contains(gone.Body.String(), "We ship on Friday") {
		t.Errorf("after an unpublish, a body: %s", gone.Body)
	}
}

// Condition 6: indexing is off unless the publisher turns it on; the
// canonical link names the page's final address.
func TestIndexingIsOffUnlessAsked(t *testing.T) {
	s := stub(t)
	h := public()
	w := get(h, "/public/p/"+slug)
	if w.Header().Get("X-Robots-Tag") != "noindex, nofollow" || !strings.Contains(w.Body.String(), `content="noindex, nofollow"`) {
		t.Errorf("a page found by default: %v", w.Header())
	}
	if !strings.Contains(w.Body.String(), `<link rel="canonical" href="https://api.example.test/public/p/`+slug+`">`) {
		t.Errorf("no canonical link:\n%s", w.Body)
	}
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) {
		return &business.Page{Slug: slug, Title: "Launch notes", HTML: "<p>Hi</p>", Indexable: true}, nil
	}
	if w := get(h, "/public/p/"+slug); w.Header().Get("X-Robots-Tag") != "" || !strings.Contains(w.Body.String(), `content="index, follow"`) {
		t.Errorf("a page its publisher lets be found: %v", w.Header())
	}
}

// Condition 7: an address with no page, never published or unpublished, has
// the same calm page, which says nothing of any doc.
func TestAnAddressWithNoPageSaysNothing(t *testing.T) {
	s := stub(t)
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, business.ErrNotFound }
	h := public()
	a, b, c := get(h, "/public/p/never-published"), get(h, "/public/p/"+slug), get(h, "/public/p/NOT%20A%20SLUG")
	if a.Code != http.StatusNotFound || !bytes.Equal(a.Body.Bytes(), b.Body.Bytes()) || !bytes.Equal(a.Body.Bytes(), c.Body.Bytes()) {
		t.Errorf("missing pages differ: %d\n%s\n%s", a.Code, a.Body, b.Body)
	}
	if strings.Contains(a.Body.String(), "never-published") || strings.Contains(a.Body.String(), slug) || !strings.Contains(a.Body.String(), "There&#39;s no page here") {
		t.Errorf("the missing page:\n%s", a.Body)
	}
}

// The other answers: a password page sends the reader to the app to unlock
// it, an expired page is gone, a copyable page offers its copy (with the
// unlock on a password page).
func TestThePagesOtherAnswers(t *testing.T) {
	s := stub(t)
	h := public()
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, business.ErrLocked }
	if w := get(h, "/public/p/"+slug+"?k=old"); w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), `href="https://app.example.test/p/`+slug+`?unlock=1"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a locked page: %d\n%s", w.Code, w.Body)
	}
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, business.ErrGone }
	if w := get(h, "/public/p/"+slug); w.Code != http.StatusGone {
		t.Errorf("an expired page: %d", w.Code)
	}
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, errors.New("down") }
	if w := get(h, "/public/p/"+slug); w.Code != http.StatusServiceUnavailable || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("a page that couldn't be read: %d", w.Code)
	}
	s.page = func(_ context.Context, _ business.Ref, token string) (*business.Page, error) {
		return &business.Page{Slug: slug, Title: "Launch notes", HTML: "<p>Hi</p>", AllowDuplicate: true, Token: token}, nil
	}
	w := get(h, "/public/p/"+slug+"?k=3.99.sig")
	if !strings.Contains(w.Body.String(), `href="https://app.example.test/p/`+slug+`?copy=1&amp;k=3.99.sig"`) ||
		w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("ETag") != "" {
		t.Errorf("an unlocked page that may be copied: %v\n%s", w.Header(), w.Body)
	}
}

func TestUnlockingAPage(t *testing.T) {
	s := stub(t)
	h := public()
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/public/p/"+slug+"/unlock", strings.NewReader(body))
		r.RemoteAddr = "203.0.113.9:40000"
		r.Header.Set("Cookie", "access_token=member")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var gotIP, gotPassword string
	s.unlock = func(_ context.Context, _ business.Ref, password, ip string) (string, time.Duration, error) {
		gotIP, gotPassword = ip, password
		if password != "open sesame" {
			return "", 0, business.ErrWrongPassword
		}
		return "3.99.sig", 12 * time.Hour, nil
	}
	w := post(`{"password":"open sesame"}`)
	var got struct {
		Data struct {
			Token     string `json:"token"`
			URL       string `json:"url"`
			ExpiresIn int    `json:"expires_in"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	out := got.Data
	if w.Code != http.StatusOK || out.URL != "https://api.example.test/public/p/"+slug+"?k=3.99.sig" || out.Token != "3.99.sig" ||
		out.ExpiresIn != 43200 || gotIP != "203.0.113.9" || gotPassword != "open sesame" {
		t.Errorf("unlocked: %d %s (%s)", w.Code, w.Body, gotIP)
	}
	if len(w.Result().Header.Values("Set-Cookie")) != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", w.Header())
	}
	if w := post(`{"password":"wrong"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("a wrong password: %d", w.Code)
	}
	if w := post(`not json`); w.Code != http.StatusBadRequest {
		t.Errorf("rubbish: %d", w.Code)
	}
	s.unlock = func(context.Context, business.Ref, string, string) (string, time.Duration, error) {
		return "", 0, business.ErrBusy
	}
	if w := post(`{"password":"x"}`); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Errorf("too many tries: %d", w.Code)
	}
}

type seeker struct{ *bytes.Reader }

func (seeker) Close() error { return nil }

func TestAPagesFiles(t *testing.T) {
	s := stub(t)
	h := public()
	var priv bool
	s.file = func(_ context.Context, _ business.Ref, obj, _ string) (*business.File, error) {
		return &business.File{ObjKey: "u1/" + obj + "_Q4 plan.pdf", ETag: `"` + obj + `"`, Private: priv}, nil
	}
	kind := "image/png"
	opened := 0
	s.stored = func(context.Context, string) (*storedFile, error) {
		opened++
		return &storedFile{Body: seeker{bytes.NewReader([]byte("\x89PNG pixels"))}, ContentType: kind, Modified: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}, nil
	}
	w := get(h, "/public/p/"+slug+"/files/"+fileA)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Content-Disposition") != "inline" ||
		w.Body.String() != "\x89PNG pixels" {
		t.Errorf("a picture: %d %v", w.Code, w.Header())
	}
	// The page has an origin of its own, so the same-site policy the API
	// gives everything else would refuse it the picture.
	if w.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || w.Header().Get("Cache-Control") != "no-cache" ||
		w.Header().Get("ETag") != `"`+fileA+`"` || w.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox;") || len(w.Result().Header.Values("Set-Cookie")) != 0 {
		t.Errorf("a picture's headers: %v", w.Header())
	}
	before := opened
	if w := get(h, "/public/p/"+slug+"/files/"+fileA, func(r *http.Request) { r.Header.Set("If-None-Match", `"`+fileA+`"`) }); w.Code != http.StatusNotModified || opened != before {
		t.Errorf("an unchanged picture: %d (opened %d)", w.Code, opened-before)
	}

	kind = "text/html; charset=utf-8"
	w = get(h, "/public/p/"+slug+"/files/"+fileA)
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Content-Disposition") != `attachment; filename="Q4 plan.pdf"` {
		t.Errorf("anything else is a download: %v", w.Header())
	}

	priv = true
	w = get(h, "/public/p/"+slug+"/files/"+fileA+"?k=3.99.sig")
	if w.Header().Get("Cache-Control") != "private, no-store" || w.Header().Get("ETag") != "" {
		t.Errorf("a password page's file: %v", w.Header())
	}

	s.stored = func(context.Context, string) (*storedFile, error) { return nil, errNoStoredFile }
	if w := get(h, "/public/p/"+slug+"/files/"+fileA); w.Code != http.StatusNotFound {
		t.Errorf("a file storage lost: %d", w.Code)
	}
	s.file = func(context.Context, business.Ref, string, string) (*business.File, error) {
		return nil, business.ErrLocked
	}
	if w := get(h, "/public/p/"+slug+"/files/"+fileA); w.Code != http.StatusNotFound {
		t.Errorf("a locked page's file without its unlock: %d", w.Code)
	}
}

func TestAFilesOwnName(t *testing.T) {
	for key, want := range map[string]string{
		"7/" + fileA + "_Q4 plan.pdf": "Q4 plan.pdf", "7/short.txt": "short.txt", "": "file", "7/": "7",
	} {
		if got := fileName(key); got != want {
			t.Errorf("fileName(%q) = %q, want %q", key, got, want)
		}
	}
}

// A doc's view-only link is the same page by its token: one renderer, the
// same headers, no canonical address (a token is never to be found), its
// unlock and its files on its own path.
func TestALinksPageByItsToken(t *testing.T) {
	s := stub(t)
	h := public()
	var refs []business.Ref
	s.page = func(_ context.Context, ref business.Ref, token string) (*business.Page, error) {
		refs = append(refs, ref)
		return &business.Page{Title: "Launch notes", HTML: "<p>Hi</p>", UpdatedAt: time.Now().Add(-5 * time.Minute)}, nil
	}
	w := get(h, "/public/d/AbC_dEf-123")
	if w.Code != http.StatusOK || len(refs) != 1 || refs[0].Token != "AbC_dEf-123" || refs[0].Slug != "" {
		t.Fatalf("the token's page: %d %+v", w.Code, refs)
	}
	body := w.Body.String()
	if strings.Contains(body, `rel="canonical"`) || w.Header().Get("X-Robots-Tag") != "noindex, nofollow" ||
		!strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox allow-downloads; default-src 'none';") {
		t.Errorf("headers %v\n%s", w.Header(), body)
	}
	if !strings.Contains(body, `Updated 5 minutes ago · <a href="/public/d/AbC_dEf-123">Reload</a> for the latest`) {
		t.Errorf("the freshness line:\n%s", body)
	}

	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, business.ErrLocked }
	if w := get(h, "/public/d/AbC_dEf-123"); w.Code != http.StatusUnauthorized ||
		!strings.Contains(w.Body.String(), `href="https://app.example.test/guest/d/AbC_dEf-123?unlock=1"`) {
		t.Errorf("a locked link's page: %d\n%s", w.Code, w.Body)
	}

	var fileRef business.Ref
	s.file = func(_ context.Context, ref business.Ref, obj, _ string) (*business.File, error) {
		fileRef = ref
		return nil, business.ErrNotFound
	}
	if w := get(h, "/public/d/AbC_dEf-123/files/"+fileA); w.Code != http.StatusNotFound || fileRef.Token != "AbC_dEf-123" {
		t.Errorf("a link's file: %d %+v", w.Code, fileRef)
	}

	var unlockRef business.Ref
	s.unlock = func(_ context.Context, ref business.Ref, _, _ string) (string, time.Duration, error) {
		unlockRef = ref
		return "3.99.sig", time.Hour, nil
	}
	r := httptest.NewRequest(http.MethodPost, "/public/d/AbC_dEf-123/unlock", strings.NewReader(`{"password":"x"}`))
	r.RemoteAddr = "203.0.113.9:40000"
	uw := httptest.NewRecorder()
	h.ServeHTTP(uw, r)
	if uw.Code != http.StatusOK || unlockRef.Token != "AbC_dEf-123" || !strings.Contains(uw.Body.String(), "/public/d/AbC_dEf-123?k=3.99.sig") {
		t.Errorf("a link's unlock: %d %+v %s", uw.Code, unlockRef, uw.Body)
	}

	// A copy goes by the page's published address, the option that lets it.
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) {
		return &business.Page{Slug: "launch", Title: "Launch notes", HTML: "<p>Hi</p>", AllowDuplicate: true}, nil
	}
	if w := get(h, "/public/d/AbC_dEf-123"); !strings.Contains(w.Body.String(), `href="https://app.example.test/p/launch?copy=1"`) {
		t.Errorf("a published link's copy:\n%s", w.Body)
	}
}

// An address's state is JSON for the web's /p, counted with the page's asks
// from the same address, and refused like the page when that's spent.
func TestAnAddressStateIsCountedWithItsPage(t *testing.T) {
	s := stub(t)
	h := public()
	prev := pageState
	t.Cleanup(func() { pageState = prev })
	pageState = func(context.Context, business.Ref) (string, error) { return business.StateLocked, nil }
	w := get(h, "/public/p/"+slug+"/state")
	if w.Code != http.StatusOK || !strings.Contains(strings.Join(strings.Fields(w.Body.String()), ""), `"state":"locked"`) {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" || strings.Join(s.ips, ",") != "page|203.0.113.9" {
		t.Fatalf("cache %q, counted %v", w.Header().Get("Cache-Control"), s.ips)
	}
	s.allow = false
	if w := get(h, "/public/p/"+slug+"/state"); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", w.Code)
	}
}

// C6-3: a page that is served counts its visitor, by the doc and its link,
// under the visitor's real address and with their privacy signals; a page
// that isn't served (locked, gone, missing, busy) counts nobody.
func TestAServedPageCountsItsVisitor(t *testing.T) {
	s := stub(t)
	h := public()
	get(h, "/public/p/"+slug, func(r *http.Request) {
		r.Header.Set("X-Forwarded-For", "198.51.100.7")
		r.Header.Set("Sec-GPC", "1")
	})
	get(h, "/public/p/"+slug, func(r *http.Request) { r.Header.Set("DNT", "1") })
	want := "doc|doc-1|grant-1|203.0.113.9|gpc=true|dnt=false,doc|doc-1|grant-1|203.0.113.9|gpc=false|dnt=true"
	if got := strings.Join(s.visits, ","); got != want {
		t.Fatalf("counted %v, want %s", s.visits, want)
	}
	s.visits = nil
	for _, err := range []error{business.ErrLocked, business.ErrGone, business.ErrNotFound, business.ErrBusy} {
		s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return nil, err }
		get(h, "/public/p/"+slug)
	}
	s.page = func(context.Context, business.Ref, string) (*business.Page, error) { return &business.Page{}, nil }
	s.allow = false
	get(h, "/public/p/"+slug)
	if len(s.visits) != 0 {
		t.Fatalf("a page that wasn't served counted %v", s.visits)
	}
}
