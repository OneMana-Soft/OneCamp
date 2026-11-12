package business

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	githubLinkModel "github.com/akashc777/OneCamp/models/postgres/GitHubLink"
	"github.com/google/uuid"
)

// GitHub is today's functions behind the interface; a branch isn't made here
// yet.
func TestGitHubIsTodaysFunctions(t *testing.T) {
	oldLinked, oldPR, oldRefresh := linkedRepos, openPR, refresh
	t.Cleanup(func() { linkedRepos, openPR, refresh = oldLinked, oldPR, oldRefresh })
	project, task := uuid.New(), uuid.New()
	var refreshed []bool
	linkedRepos = func(_ context.Context, id uuid.UUID) ([]*githubLinkModel.GitHubLink, error) {
		if id != project {
			t.Errorf("asked about %s", id)
		}
		return []*githubLinkModel.GitHubLink{{RepoOwner: "acme", RepoName: "web"}, nil}, nil
	}
	openPR = func(_ context.Context, id uuid.UUID, title, body, head string) (*struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}, error) {
		return &struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
		}{Number: 42, HTMLURL: "https://github.com/acme/web/pull/42"}, nil
	}
	var refresher *dgraphStruct.DgraphUser
	refresh = func(_ context.Context, id uuid.UUID, backfill bool, by *dgraphStruct.DgraphUser) error {
		refreshed, refresher = append(refreshed, backfill), by
		return nil
	}
	gh := Providers[GitHub]
	repos, err := gh.LinkedRepos(context.Background(), project)
	if err != nil || len(repos) != 1 || repos[0] != (Repo{Owner: "acme", Name: "web"}) {
		t.Errorf("repos %v %v", repos, err)
	}
	pr, err := gh.OpenPullRequest(context.Background(), task, "Fix it", "", "fix-it")
	if err != nil || pr.Number != 42 || pr.URL == "" {
		t.Errorf("pr %+v %v", pr, err)
	}
	ana := &dgraphStruct.DgraphUser{Uid: "0xana"}
	if err := gh.Refresh(context.Background(), task, ana); err != nil || len(refreshed) != 1 || refreshed[0] || refresher != ana {
		t.Errorf("refresh %v %v by %v: once, without the older comments, as whoever refreshed", refreshed, err, refresher)
	}
	if err := gh.MakeBranch(context.Background(), task, "fix-it"); !errors.Is(err, ErrNotBuilt) {
		t.Errorf("branch: %v", err)
	}
}

// githubHook is a delivery as GitHub sends one.
func githubHook(sig, delivery string) http.Header {
	h := http.Header{}
	h.Set("X-GitHub-Event", "push")
	if sig != "" {
		h.Set("X-Hub-Signature-256", sig)
	}
	if delivery != "" {
		h.Set("X-GitHub-Delivery", delivery)
	}
	return h
}

// R2: every delivery is let in once, by its signature and its id, before
// anything reads it: an unsigned or badly signed one is refused without
// claiming its id, one with no id is refused, and a second delivery of the
// same id (a replay) is refused, so it moves nothing twice.
func TestGitHubVerifiesSignatureThenClaimsTheDelivery(t *testing.T) {
	oldVerify, oldClaim := verifySignature, claimDelivery
	t.Cleanup(func() { verifySignature, claimDelivery = oldVerify, oldClaim })
	body := []byte(`{"repository":{"name":"web","owner":{"login":"acme"}}}`)
	verifySignature = func(_ context.Context, b []byte, sig, event string) (bool, *githubLinkModel.GitHubLink) {
		return sig == "sha256=good" && string(b) == string(body) && event == "push", nil
	}
	claimed := map[string]int{}
	claimDelivery = func(_ context.Context, delivery, event string) (bool, error) {
		claimed[delivery]++
		if delivery == "broken" {
			return false, errors.New("the database is down")
		}
		return claimed[delivery] == 1, nil
	}
	gh := Providers[GitHub]
	ctx := context.Background()

	for name, h := range map[string]http.Header{"unsigned": githubHook("", "d-1"), "badly signed": githubHook("sha256=bad", "d-1")} {
		if _, err := gh.Verify(ctx, h, body); !errors.Is(err, ErrUnsigned) {
			t.Errorf("%s: %v, want ErrUnsigned", name, err)
		}
	}
	if claimed["d-1"] != 0 {
		t.Errorf("a delivery that failed its signature claimed its id")
	}
	if _, err := gh.Verify(ctx, githubHook("sha256=good", ""), body); !errors.Is(err, ErrNoDelivery) {
		t.Errorf("no delivery id: %v, want ErrNoDelivery", err)
	}
	hook, err := gh.Verify(ctx, githubHook("sha256=good", "d-1"), body)
	if err != nil || hook.Event != "push" || hook.Delivery != "d-1" || string(hook.Body) != string(body) {
		t.Errorf("first delivery: %+v %v", hook, err)
	}
	if _, err := gh.Verify(ctx, githubHook("sha256=good", "d-1"), body); !errors.Is(err, ErrReplayed) {
		t.Errorf("replayed delivery: %v, want ErrReplayed", err)
	}
	if _, err := gh.Verify(ctx, githubHook("sha256=good", "broken"), body); err == nil || errors.Is(err, ErrReplayed) {
		t.Errorf("a claim that failed: %v, want its error", err)
	}
}

// A verified delivery is handled once and its outcome kept: done, or failed
// with why (a panic included), so GitHub may deliver a failed one again.
func TestGitHubHandleRecordsTheOutcome(t *testing.T) {
	oldHandle, oldDone, oldFailed := handleEvent, deliveryDone, deliveryFailed
	t.Cleanup(func() { handleEvent, deliveryDone, deliveryFailed = oldHandle, oldDone, oldFailed })
	var done, failed []string
	deliveryDone = func(_ context.Context, delivery string) error { done = append(done, delivery); return nil }
	deliveryFailed = func(_ context.Context, delivery, why string) error {
		failed = append(failed, delivery+": "+why)
		return nil
	}
	handleEvent = func(_ context.Context, event string, body []byte) error {
		switch string(body) {
		case "fails":
			return errors.New("no luck")
		case "panics":
			panic("boom")
		}
		return nil
	}
	gh := Providers[GitHub]
	ctx := context.Background()
	if err := gh.Handle(ctx, Hook{Event: "push", Delivery: "ok", Body: []byte("{}")}); err != nil {
		t.Errorf("ok: %v", err)
	}
	if err := gh.Handle(ctx, Hook{Event: "push", Delivery: "bad", Body: []byte("fails")}); err == nil {
		t.Error("a failed event answered no error")
	}
	if err := gh.Handle(ctx, Hook{Event: "push", Delivery: "boom", Body: []byte("panics")}); err == nil {
		t.Error("a panic answered no error")
	}
	if !reflect.DeepEqual(done, []string{"ok"}) || len(failed) != 2 || !strings.HasPrefix(failed[0], "bad: no luck") || !strings.Contains(failed[1], "boom") {
		t.Errorf("done %v failed %v", done, failed)
	}
}
