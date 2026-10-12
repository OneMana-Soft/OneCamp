package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"
)

const (
	readableDoc   = "aaaaaaaa-0000-4000-8000-000000000001"
	unreadableDoc = "aaaaaaaa-0000-4000-8000-000000000002"
	goneDoc       = "aaaaaaaa-0000-4000-8000-000000000003"
)

func withReadable(t *testing.T, fn func(ctx context.Context, docs []string, userUUID string) (map[string]bool, error)) {
	t.Helper()
	old := readableDocs
	readableDocs = fn
	t.Cleanup(func() { readableDocs = old })
}

func docHit(id string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.DOC_TYPE, Doc: &openSearchStruct.OpenSearchDoc{Uuid: id}}
}

func commentOn(doc string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.COMMENT_TYPE,
		Comment: &openSearchStruct.OpenSearchComment{Uuid: "c-" + doc, CommentDocUuid: doc}}
}

func fileIn(doc string) *openSearchStruct.GlobalSearchOpenSearchResp {
	return &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.ATTACHMENT_TYPE,
		Attachment: &openSearchStruct.OpenSearchAttachment{Uuid: "f-" + doc, AttachmentDocUuid: doc}}
}

// Someone taken off a private doc kept finding its title and text in global
// search: the index's copy of who it was shared with kept them on it, and
// global search, unlike AI search, never asked the doc. Every hit on a doc, or
// on a comment or file in one, is now kept only if the doc says the person may
// read it now.
func TestSearchKeepsOnlyTheDocsThePersonCanRead(t *testing.T) {
	var askedFor string
	var askedAbout []string
	withReadable(t, func(_ context.Context, docs []string, userUUID string) (map[string]bool, error) {
		askedFor, askedAbout = userUUID, docs
		return map[string]bool{readableDoc: true, unreadableDoc: false}, nil
	})
	chatComment := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.COMMENT_TYPE, Comment: &openSearchStruct.OpenSearchComment{Uuid: "chat"}}
	board := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.BOARD_TYPE, Board: &openSearchStruct.OpenSearchBoard{Uuid: "b"}}
	hits := []*openSearchStruct.GlobalSearchOpenSearchResp{
		docHit(readableDoc), docHit(unreadableDoc), docHit(goneDoc),
		commentOn(unreadableDoc), commentOn(readableDoc), fileIn(unreadableDoc), fileIn(readableDoc),
		task(liveTask), chatComment, board,
	}
	got := keepReadableDocHits(context.Background(), "me", hits)
	want := []*openSearchStruct.GlobalSearchOpenSearchResp{hits[0], hits[4], hits[6], hits[7], hits[8], hits[9]}
	if len(got) != len(want) {
		t.Fatalf("kept %d hits, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("hit %d: kept %+v, want %+v", i, got[i], want[i])
		}
	}
	if askedFor != "me" || len(askedAbout) != 3 {
		t.Errorf("asked about %v for %q, want the three docs, once each, for the person searching", askedAbout, askedFor)
	}
}

// A graph that can't be asked keeps no doc hit (refusing costs a result,
// keeping a wrong one costs a private doc), and every other hit.
func TestSearchLeavesDocsOutWhenTheGraphCannotBeAsked(t *testing.T) {
	withReadable(t, func(context.Context, []string, string) (map[string]bool, error) {
		return nil, errors.New("graph down")
	})
	got := keepReadableDocHits(context.Background(), "me", []*openSearchStruct.GlobalSearchOpenSearchResp{
		docHit(readableDoc), commentOn(readableDoc), task(liveTask)})
	if len(got) != 1 || got[0].Task == nil {
		t.Fatalf("kept %d hits with the graph down, want only the task", len(got))
	}
}

// The search bar's search (and the AI's name search, and /v1/search, which run
// it) puts every page through that check: a private doc the index still grants
// someone is left out of what they're shown.
func TestUnifiedSearchChecksEveryDocHitWithTheGraph(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"hits":{"hits":[
			{"_index":"docs","_id":%q,"_source":{"doc_uuid":%q,"doc_title":"Salaries","doc_private":true,"doc_reading_users":["me"]}},
			{"_index":"tasks","_id":%q,"_source":{"task_id":%q,"task_name":"Salaries review"}}]}}`,
			unreadableDoc, unreadableDoc, liveTask, liveTask)
	}))
	defer node.Close()
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{node.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	oldClient := opensearchInit.OpenSearchClient
	opensearchInit.OpenSearchClient = client
	t.Cleanup(func() { opensearchInit.OpenSearchClient = oldClient })
	withChecks(t, map[string]liveness.Checker{}) // nothing gone
	var asked string
	withReadable(t, func(_ context.Context, docs []string, userUUID string) (map[string]bool, error) {
		asked = userUUID
		return map[string]bool{unreadableDoc: false}, nil
	})

	p, err := GetUnifiedGlobalSearch(context.Background(), "me", "me@example.test", nil, nil, nil, "salaries")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Page) != 1 || p.Page[0].Task == nil {
		t.Fatalf("the page holds %d hits; want only the task, not the doc they can't read", len(p.Page))
	}
	if asked != "me" {
		t.Errorf("the doc was checked for %q, want the person searching", asked)
	}
}
