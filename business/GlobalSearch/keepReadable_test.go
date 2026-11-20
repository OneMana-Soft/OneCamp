package business

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	liveness "github.com/akashc777/OneCamp/domain/Liveness"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// pageChecks stands in for the five read checks a page goes through
// (transcripts, docs, boards, events, project files), each answering yes for
// what it is asked about unless it is the one named to fail. arrive runs as
// each check starts.
func pageChecks(t *testing.T, fail string, arrive func()) {
	t.Helper()
	broken := errors.New("the store is down")
	swap(t, &liveCheckers, map[string]liveness.Checker{})
	swap(t, &readableDocs, func(_ context.Context, ids []string, _ string) (map[string]bool, error) {
		arrive()
		if fail == "docs" {
			return nil, broken
		}
		return map[string]bool{"d1": true}, nil
	})
	swap(t, &readableBoards, func(_ context.Context, ids []string, _ string) (map[string]bool, error) {
		arrive()
		if fail == "boards" {
			return nil, broken
		}
		return map[string]bool{"b1": true}, nil
	})
	swap(t, &readableEvents, func(_ context.Context, _ accessBusiness.Subject, ids []string) (map[string]bool, error) {
		arrive()
		if fail == "events" {
			return nil, broken
		}
		return map[string]bool{"e1": true}, nil
	})
	swap(t, &mayReadFiles, func(_ context.Context, _ string, _ fileAccessBusiness.Place) error {
		arrive()
		if fail == "files" {
			return broken
		}
		return nil
	})
	swap(t, &transcriptHomesOf, func(context.Context, []string, []string) (map[string]recordingDomain.TranscriptHome, error) {
		arrive()
		if fail == "transcripts" {
			return nil, broken
		}
		return map[string]recordingDomain.TranscriptHome{"EG_x": {ConvID: "X", Ended: true, Lines: map[string]bool{"0x3": true}}}, nil
	})
}

// aPageOfEveryKind is a page with one hit each check asks about, and a
// message no check asks about.
func aPageOfEveryKind() (Reach, []*openSearchStruct.GlobalSearchOpenSearchResp) {
	post := &openSearchStruct.GlobalSearchOpenSearchResp{Type: openSearchStruct.POST_TYPE, Post: &openSearchStruct.OpenSearchPost{Uuid: "m1"}}
	return Reach{UserUUID: "me", Calls: &CallReach{Places: []string{"X"}}}, []*openSearchStruct.GlobalSearchOpenSearchResp{
		post, docHit("d1"), boardHit("b1"), eventHit("e1"), projectFile("p9"), transcriptHit("0x3", "EG_x", "X"),
	}
}

func keptKinds(hits []*openSearchStruct.GlobalSearchOpenSearchResp) string {
	var kinds []string
	for _, h := range hits {
		kinds = append(kinds, h.Type)
	}
	return strings.Join(kinds, ",")
}

// D7: a page's five read checks run at once, on the same page, not one after
// another. Each check here waits until all five have started: checks made one
// after another each wait out the timer.
func TestAPagesReadChecksRunAtOnce(t *testing.T) {
	var arrived atomic.Int32
	var late atomic.Int32
	all := make(chan struct{})
	var once sync.Once
	pageChecks(t, "", func() {
		if arrived.Add(1) == 5 {
			once.Do(func() { close(all) })
		}
		select {
		case <-all:
		case <-time.After(300 * time.Millisecond):
			late.Add(1)
		}
	})
	reach, hits := aPageOfEveryKind()
	kept := keepPage(context.Background(), reach, hits)
	if got, want := keptKinds(kept), keptKinds(hits); got != want {
		t.Errorf("kept %s, want every hit in its order: %s", got, want)
	}
	if n := late.Load(); n > 0 {
		t.Errorf("%d of the 5 checks waited for the others: they ran one after another", n)
	}
}

// A check that fails leaves its own kind out and every other hit in, in the
// page's order: no check's failure reaches another's kind (fail closed per
// kind). A request cancelled while the checks ran keeps nothing.
func TestAFailedPageCheckLeavesOnlyItsKindOut(t *testing.T) {
	reach, hits := aPageOfEveryKind()
	kindOf := map[string]string{
		"docs": openSearchStruct.DOC_TYPE, "boards": openSearchStruct.BOARD_TYPE, "events": openSearchStruct.EVENT_TYPE,
		"files": openSearchStruct.ATTACHMENT_TYPE, "transcripts": openSearchStruct.TRANSCRIPT_TYPE,
	}
	for fail, kind := range kindOf {
		pageChecks(t, fail, func() {})
		var want []string
		for _, h := range hits {
			if h.Type != kind {
				want = append(want, h.Type)
			}
		}
		if got := keptKinds(keepPage(context.Background(), reach, hits)); got != strings.Join(want, ",") {
			t.Errorf("the %s check failed: kept %s, want %s", fail, got, strings.Join(want, ","))
		}
	}
	pageChecks(t, "", func() {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := keepPage(ctx, reach, hits); len(got) != 0 {
		t.Errorf("a cancelled request kept %s, want nothing", keptKinds(got))
	}
}
