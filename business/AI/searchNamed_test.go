package business

import (
	"strings"
	"testing"

	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// The demo: "Launch sync notes" returned four copies of the message that named
// it and never the doc, and no hit carried an id a tool could take.
func TestSearchFindsTheNamedDocOnceWithItsId(t *testing.T) {
	jonas := "@Sam Rivera can we get the rollback steps into the Launch sync notes before Thursday?"
	semantic := &UnifiedSearchResponse{Enabled: true, Query: "Launch sync notes", Groups: []UnifiedSearchGroup{{
		Source: UnifiedSourceWorkspace, Label: "Workspace",
		Hits: []UnifiedHit{
			{Title: "Post by Jonas Weber", Snippet: jonas, ContentType: "post", ChannelUUID: "c-1", PostUUID: "p-1"},
			{Title: "Post by Jonas Weber", Snippet: jonas, ContentType: "post", ChannelUUID: "c-2", PostUUID: "p-2"},
			{Title: "Post by Jonas Weber", Snippet: jonas, ContentType: "post", ChannelUUID: "c-3", PostUUID: "p-3"},
			{Title: "Post by Maya", Snippet: "ok", ContentType: "post", ChannelUUID: "c-1", PostUUID: "p-4"},
			{Title: "Post by Jonas", Snippet: "ok", ContentType: "post", ChannelUUID: "c-1", PostUUID: "p-5"},
		},
	}}}
	named := namedHits([]*openSearchStruct.GlobalSearchOpenSearchResp{
		{Doc: &openSearchStruct.OpenSearchDoc{Uuid: "d-1", DocTitle: "Launch sync notes"}},
		{Task: &openSearchStruct.OpenSearchTask{Uuid: "t-1", TaskName: "Add the rollback steps", TaskProjectUuid: "pr-1", TaskProjectName: "Q4 launch"}},
		{Post: &openSearchStruct.OpenSearchPost{}}, // a message: semantic recall covers these
		nil,
	}, 6)
	if len(named) != 2 {
		t.Fatalf("named hits = %d, want the doc and the task", len(named))
	}

	out := renderUnifiedSearchForModel(withNamedHits(semantic, named))
	if !strings.Contains(out, "Doc: Launch sync notes [doc_uuid=d-1]") {
		t.Fatalf("the doc is missing or has no id:\n%s", out)
	}
	if !strings.Contains(out, "[task_uuid=t-1 project_uuid=pr-1]") {
		t.Fatalf("the task has no ids:\n%s", out)
	}
	if n := strings.Count(out, jonas); n != 1 {
		t.Fatalf("the same message appears %d times, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, "\n  ok"); n != 2 {
		t.Fatalf("two different short replies must both stay, got %d:\n%s", n, out)
	}
	if strings.Index(out, "Launch sync notes [doc_uuid") > strings.Index(out, "Post by Jonas Weber") {
		t.Fatal("named matches must come first")
	}
}

func TestHitRef(t *testing.T) {
	cases := map[string]UnifiedHit{
		"doc_uuid=d":                 {DocUUID: "d"},
		"channel_uuid=c post_uuid=p": {ChannelUUID: "c", PostUUID: "p"},
		"project_uuid=pr":            {ContentType: "project", ProjectUUID: "pr"},
		"channel_uuid=c":             {ContentType: "channel", ChannelUUID: "c"},
		"chat_grp_id=g":              {ChatGrpID: "g"},
		"board_uuid=b":               {ContentType: "board", ContentUUID: "b"},
		"":                           {Title: "An email", URL: "https://mail.example/x"},
	}
	for want, h := range cases {
		if got := hitRef(h); got != want {
			t.Fatalf("hitRef(%+v) = %q, want %q", h, got, want)
		}
	}
}

// The search bar's order for "Launch sync notes" on the demo: the project
// "Q4 launch", five tasks, then the doc with that exact name, which a first-six
// cut dropped.
func TestRankNamedHitsPutsTheExactNameFirst(t *testing.T) {
	var hits []UnifiedHit
	hits = append(hits, UnifiedHit{Title: "Project: Q4 launch", ContentType: "project", ProjectUUID: "pr"})
	for i := 0; i < 5; i++ {
		hits = append(hits, UnifiedHit{Title: "Task: Add the rollback steps to the Launch sync notes", ContentType: "task", TaskUUID: string(rune('a' + i))})
	}
	hits = append(hits, UnifiedHit{Title: "Doc: Launch sync notes", ContentType: "doc", DocUUID: "d"})

	got := rankNamedHits(hits, "Launch sync notes", 6)
	if len(got) != 6 || got[0].DocUUID != "d" {
		t.Fatalf("the exact name must lead and survive the cut: %+v", got)
	}
	if got[5].TaskUUID == "" {
		t.Fatalf("partial matches come after the exact one, and a one-word match last: %+v", got)
	}
}

func TestNameMatchScore(t *testing.T) {
	for _, c := range []struct {
		title, q string
		want     int
	}{
		{"Launch sync notes", "launch  sync notes", 3},
		{"Add the rollback steps to the Launch sync notes", "Launch sync notes", 2},
		{"Notes from the launch sync", "Launch sync notes", 1},
		{"Q4 launch", "Launch sync notes", 0},
		{"anything", "", 0},
	} {
		if got := nameMatchScore(c.title, c.q); got != c.want {
			t.Fatalf("nameMatchScore(%q, %q) = %d, want %d", c.title, c.q, got, c.want)
		}
	}
}
