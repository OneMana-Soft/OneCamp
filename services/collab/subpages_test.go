package collab

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A page's block ops name the page, in the mover's name, and say what they
// did: how many blocks went, and the one that landed.
func TestAPagesBlockOpsNameThePageAndSayWhatTheyDid(t *testing.T) {
	const page = "7a1e0c52-4b8d-4f3e-9a6b-2c5d8e1f0a37"
	_, bodies := fakeLive(t, func(op string, _ map[string]any) (int, any) {
		if op == "removeSubpage" {
			return http.StatusOK, map[string]any{"blocks": 0, "removed": 2, "persisted": true}
		}
		return http.StatusOK, map[string]any{"blocks": 1, "block_ids": []string{"b9"}, "persisted": true}
	})
	by := Options{By: By{UserUUID: "sam"}}
	gone, err := RemoveSubpage(context.Background(), "old-parent", page, by)
	if err != nil || gone.Removed != 2 {
		t.Fatalf("remove: %+v %v", gone, err)
	}
	placed, err := PlaceSubpage(context.Background(), "new-parent", page, by)
	if err != nil || placed.Blocks != 1 || len(placed.BlockIDs) != 1 || placed.BlockIDs[0] != "b9" {
		t.Fatalf("place: %+v %v", placed, err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("sent %d requests, want 2", len(*bodies))
	}
	for i, want := range []string{"/live/docs/old-parent/removeSubpage", "/live/docs/new-parent/placeSubpage"} {
		b := (*bodies)[i]
		if b["_path"] != want || b["doc_id"] != page || b["by"].(map[string]any)["user_uuid"] != "sam" {
			t.Errorf("request %d: %v, want %s naming the page, by sam", i, b, want)
		}
	}
}

// Nothing to take, or the block there already, is a refusal the caller tells
// apart from a failure: the doc is as the tree says either way.
func TestAPagesBlockAlreadyGoneOrThereIsSaidSo(t *testing.T) {
	fakeLive(t, func(op string, _ map[string]any) (int, any) {
		if op == "removeSubpage" {
			return http.StatusNotFound, "no block of that page"
		}
		return http.StatusConflict, "the doc holds a block of that page already"
	})
	if _, err := RemoveSubpage(context.Background(), "old-parent", "p", Options{}); !errors.Is(err, ErrNoSuchBlock) || errors.Is(err, ErrNoSuchDoc) {
		t.Errorf("remove with no block: %v, want ErrNoSuchBlock", err)
	}
	if _, err := PlaceSubpage(context.Background(), "new-parent", "p", Options{}); !errors.Is(err, ErrHasContent) {
		t.Errorf("place with the block there: %v, want ErrHasContent", err)
	}
}

// A service older than the API answers an op it hasn't got with "no such op":
// a write it can't make now, which the caller logs, never a doc that holds no
// block of the page, which it lets be.
func TestAnOpTheServiceHasNotGotIsUnavailable(t *testing.T) {
	fakeLive(t, func(string, map[string]any) (int, any) { return http.StatusNotFound, "no such op" })
	_, err := RemoveSubpage(context.Background(), "old-parent", "p", Options{})
	if !errors.Is(err, ErrUnavailable) || errors.Is(err, ErrNoSuchBlock) || errors.Is(err, ErrRefused) {
		t.Errorf("an op the service hasn't got: %v, want ErrUnavailable alone", err)
	}
}
