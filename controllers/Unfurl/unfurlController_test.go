package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	business "github.com/akashc777/OneCamp/business/Unfurl"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// sam is a signed-in member, as VerifyAuth puts one on the request.
var sam = userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x1", Uuid: "5f1c2b9e-0a4d-4e8b-9c3f-6d7e8f9a0b1c"}}

func signedIn(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), helpers.UserInfoContextKey, sam))
}

// fakeBusiness swaps the business calls and the fetch window, and puts them
// back after the test.
func fakeBusiness(t *testing.T) {
	t.Helper()
	oldCard, oldStore, oldHosts, oldAllow := unfurlCard, storeRemote, embedHosts, allowFetch
	t.Cleanup(func() { unfurlCard, storeRemote, embedHosts, allowFetch = oldCard, oldStore, oldHosts, oldAllow })
	allowFetch = func(context.Context, string, string) (bool, int) { return true, 0 }
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestAPreviewNeedsSomeoneSignedIn(t *testing.T) {
	fakeBusiness(t)
	called := false
	unfurlCard = func(context.Context, string, func() error) (business.Card, error) {
		called = true
		return business.Card{}, nil
	}
	for name, h := range map[string]http.HandlerFunc{"Unfurl": Unfurl, "EmbedSites": EmbedSites, "StoreRemote": StoreRemote} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(http.MethodGet, "/unfurl?url=https://example.com/", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with nobody signed in: %d; want 401", name, rec.Code)
		}
	}
	if called {
		t.Error("nothing is previewed for nobody")
	}
}

func TestAPreviewIsACardEvenWhenTheSiteCantBeRead(t *testing.T) {
	fakeBusiness(t)
	cards := map[string]business.Card{
		"https://example.com/":  {URL: "https://example.com/", Title: "Example", Fetched: true},
		"https://down.example/": {URL: "https://down.example/", Reason: business.ReasonUnreachable},
	}
	var asked []string
	unfurlCard = func(_ context.Context, raw string, budget func() error) (business.Card, error) {
		asked = append(asked, raw)
		if budget == nil {
			t.Error("the person's fetch budget must reach the business")
		}
		if c, ok := cards[raw]; ok {
			return c, nil
		}
		return business.Card{}, business.ErrInvalidURL
	}

	rec := httptest.NewRecorder()
	Unfurl(rec, signedIn(httptest.NewRequest(http.MethodGet, "/unfurl?url=https://example.com/", nil)))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "private, max-age=3600" {
		t.Fatalf("a read page: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if data := decode(t, rec)["data"].(map[string]any); data["title"] != "Example" || data["fetched"] != true {
		t.Errorf("card %v", data)
	}

	rec = httptest.NewRecorder()
	Unfurl(rec, signedIn(httptest.NewRequest(http.MethodGet, "/unfurl?url=https://down.example/", nil)))
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "private, max-age=600" {
		t.Fatalf("a site that's down is a card that says so, kept briefly: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if data := decode(t, rec)["data"].(map[string]any); data["fetched"] != false || data["reason"] != business.ReasonUnreachable {
		t.Errorf("card %v", data)
	}

	rec = httptest.NewRecorder()
	Unfurl(rec, signedIn(httptest.NewRequest(http.MethodGet, "/unfurl?url=javascript:alert(1)", nil)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(decode(t, rec)["msg"].(string), "http://") {
		t.Errorf("not a web link: %d %s", rec.Code, rec.Body.String())
	}
}

func TestTooManyPreviewsInAMinuteAreRefusedWithWhenToTryAgain(t *testing.T) {
	fakeBusiness(t)
	allowFetch = func(_ context.Context, action, user string) (bool, int) {
		if action != "preview" || user != sam.UserDgraphInfo.Uuid {
			t.Errorf("counted %q for %q; want the person's previews", action, user)
		}
		return false, 42
	}
	unfurlCard = func(_ context.Context, _ string, budget func() error) (business.Card, error) {
		return business.Card{}, budget()
	}
	rec := httptest.NewRecorder()
	Unfurl(rec, signedIn(httptest.NewRequest(http.MethodGet, "/unfurl?url=https://example.com/", nil)))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "42" {
		t.Fatalf("over the limit: %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if decode(t, rec)["status"] != "rate_limited" {
		t.Errorf("body %s", rec.Body.String())
	}
}

func TestEmbedSitesAreTheAdminsList(t *testing.T) {
	fakeBusiness(t)
	for _, tc := range []struct {
		hosts []string
		want  string
	}{{nil, `[]`}, {[]string{"embed.example.com"}, `["embed.example.com"]`}} {
		embedHosts = func(context.Context) []string { return tc.hosts }
		rec := httptest.NewRecorder()
		EmbedSites(rec, signedIn(httptest.NewRequest(http.MethodGet, "/unfurl/embeds", nil)))
		got, _ := json.Marshal(decode(t, rec)["data"].(map[string]any)["hosts"])
		if rec.Code != http.StatusOK || string(got) != tc.want {
			t.Errorf("hosts %v: %d %s; want %s", tc.hosts, rec.Code, got, tc.want)
		}
	}
}

func TestStoringAPictureSaysWhyItCouldnt(t *testing.T) {
	fakeBusiness(t)
	const doc = "8c0f2a4e-1b3d-4f5a-9e6c-7d8b9a0c1e2f"
	results := map[string]error{
		"ok":       nil,
		"limited":  errLimited{retryAfter: 9},
		"bad-url":  business.ErrInvalidURL,
		"bad-kind": business.ErrBadKind,
		"no-doc":   business.ErrNoSuchDoc,
		"no-edit":  business.ErrCantEdit,
		"blocked":  &business.NotStoredError{Reason: business.ReasonBlocked},
		"svg":      &business.NotStoredError{Reason: business.ReasonNotImage},
		"odd":      &business.NotStoredError{Reason: "something new"},
		"broken":   errors.New("minio is down"),
	}
	want := map[string]int{
		"ok": 200, "limited": 429, "bad-url": 400, "bad-kind": 400, "no-doc": 404, "no-edit": 403,
		"blocked": 422, "svg": 422, "odd": 422, "broken": 503,
	}
	storeRemote = func(_ context.Context, who accessBusiness.Subject, docUUID, raw, kind string, budget func() error) (business.Stored, error) {
		if who.UserUUID != sam.UserDgraphInfo.Uuid || who.UserUID != sam.UserDgraphInfo.Uid || docUUID != doc || kind != "image" || budget == nil {
			t.Errorf("asked as %+v for %s %s %s", who, docUUID, raw, kind)
		}
		if err := results[raw]; err != nil {
			return business.Stored{}, err
		}
		return business.Stored{ObjectUUID: "obj-1", FileName: "a.png", ContentType: "image/png", Size: 12}, nil
	}
	for name, code := range want {
		body := `{"doc_uuid":"` + doc + `","url":"` + name + `","kind":"image"}`
		rec := httptest.NewRecorder()
		StoreRemote(rec, signedIn(httptest.NewRequest(http.MethodPost, "/unfurl/store", strings.NewReader(body))))
		if rec.Code != code {
			t.Errorf("%s: %d %s; want %d", name, rec.Code, rec.Body.String(), code)
			continue
		}
		out := decode(t, rec)
		switch name {
		case "ok":
			if out["data"].(map[string]any)["object_uuid"] != "obj-1" {
				t.Errorf("stored %v", out)
			}
		case "limited":
			if rec.Header().Get("Retry-After") != "9" {
				t.Errorf("Retry-After %q", rec.Header().Get("Retry-After"))
			}
		case "blocked":
			if out["reason"] != business.ReasonBlocked || !strings.Contains(out["msg"].(string), "Upload the file instead") {
				t.Errorf("%s: %v", name, out)
			}
		case "svg":
			if out["reason"] != business.ReasonNotImage || !strings.Contains(out["msg"].(string), "PNG") {
				t.Errorf("%s: %v", name, out)
			}
		case "broken":
			// A store that failed says nothing about which one.
			if strings.Contains(rec.Body.String(), "minio") {
				t.Errorf("the reply names an internal system: %s", rec.Body.String())
			}
		}
	}

	rec := httptest.NewRecorder()
	StoreRemote(rec, signedIn(httptest.NewRequest(http.MethodPost, "/unfurl/store", strings.NewReader("{not json"))))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a body that isn't JSON: %d", rec.Code)
	}
}
