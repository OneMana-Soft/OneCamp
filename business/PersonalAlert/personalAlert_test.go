package business

import (
	"context"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

func TestCleanKeywordsTrimsAndDedupes(t *testing.T) {
	got, err := CleanKeywords([]string{"  launch  plan ", "Launch Plan", "", "q4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "launch plan" || got[1] != "q4" {
		t.Fatalf("got %q", got)
	}
}

func TestCleanKeywordsRefusesBadLengthsAndTooMany(t *testing.T) {
	if _, err := CleanKeywords([]string{"x"}); err == nil {
		t.Fatal("a one-letter keyword was kept")
	}
	if _, err := CleanKeywords([]string{strings.Repeat("a", 65)}); err == nil {
		t.Fatal("a 65-character keyword was kept")
	}
	many := make([]string, MaxKeywords+1)
	for i := range many {
		many[i] = "kw" + strings.Repeat("z", i%60) + string(rune(0x61+i%26)) + string(rune(0x30+i/26))
	}
	if _, err := CleanKeywords(many); err == nil {
		t.Fatal("51 keywords were kept")
	} else if r, ok := IsRefusal(err); !ok || r.Status() != 400 {
		t.Fatalf("not a refusal: %v", err)
	}
}

func TestMatchKeywordsWholeWordsAnyCase(t *testing.T) {
	hits := matchKeywords("The LAUNCH is friday; relaunch later", map[string][]string{
		"a": {"launch"},
		"b": {"unch"},
		"c": {"nope", "Friday"},
	})
	if hits["a"] != "launch" || hits["c"] != "Friday" {
		t.Fatalf("hits %v", hits)
	}
	if _, ok := hits["b"]; ok {
		t.Fatal("a part of a word matched")
	}
}

func TestSetVIPsKeepsLivePeopleNeverSelf(t *testing.T) {
	me, live, gone := uuid.New(), uuid.New(), uuid.New()
	oldLive, oldWrite := livePeople, writeVIPs
	t.Cleanup(func() { livePeople, writeVIPs = oldLive, oldWrite })
	livePeople = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		return []*dgraphStruct.DgraphUser{{Uuid: live.String(), UserName: "Maya"}}, nil
	}
	var wrote []string
	writeVIPs = func(_ context.Context, _ uuid.UUID, ids []string) error { wrote = ids; return nil }
	got, err := SetVIPs(context.Background(), me, []string{me.String(), live.String(), live.String(), gone.String(), "junk"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UUID != live.String() || got[0].Name != "Maya" || len(wrote) != 1 || wrote[0] != live.String() {
		t.Fatalf("kept %v wrote %v", got, wrote)
	}
}
