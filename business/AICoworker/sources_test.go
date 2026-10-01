package aicoworker

import (
	"strings"
	"testing"

	aiAdapter "github.com/akashc777/OneCamp/adapter/AI"
)

func TestSourceDeepLink(t *testing.T) {
	const asker = "11111111-1111-1111-1111-111111111111"
	const peer = "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name string
		src  aiAdapter.SourceRef
		want string
	}{
		{"post in channel", aiAdapter.SourceRef{ContentType: "post", ChannelUUID: "ch1", ContentUUID: "p1"}, "/app/channel/ch1/p1"},
		{"post without msg uuid", aiAdapter.SourceRef{ContentType: "post", ChannelUUID: "ch1"}, "/app/channel/ch1"},
		{"post without channel", aiAdapter.SourceRef{ContentType: "post", ContentUUID: "p1"}, ""},
		{"doc", aiAdapter.SourceRef{ContentType: "doc", ContentUUID: "d1"}, "/app/doc/d1"},
		{"task", aiAdapter.SourceRef{ContentType: "task", ContentUUID: "t1"}, "/app/task/t1"},
		{"comment on post", aiAdapter.SourceRef{ContentType: "comment", ChannelUUID: "ch1", PostUUID: "p1"}, "/app/channel/ch1/p1"},
		{"comment on task", aiAdapter.SourceRef{ContentType: "comment", TaskUUID: "t1"}, "/app/task/t1"},
		{"comment on doc", aiAdapter.SourceRef{ContentType: "comment", DocUUID: "d1"}, "/app/doc/d1/comment"},
		{"group chat", aiAdapter.SourceRef{ContentType: "chat", ChatGrpID: "grpidnospace", ContentUUID: "c1"}, "/app/chat/group/grpidnospace/c1"},
		{"dm routes to other participant", aiAdapter.SourceRef{ContentType: "chat", ChatGrpID: asker + " " + peer, ContentUUID: "c1"}, "/app/chat/" + peer + "/c1"},
		{"dm with only self yields nothing", aiAdapter.SourceRef{ContentType: "chat", ChatGrpID: asker + " " + asker, ContentUUID: "c1"}, ""},
		{"unknown type", aiAdapter.SourceRef{ContentType: "mystery"}, ""},
	}
	for _, tc := range cases {
		if got := sourceDeepLink(tc.src, asker); got != tc.want {
			t.Fatalf("%s: sourceDeepLink = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSourcesFooterHTML(t *testing.T) {
	const asker = "11111111-1111-1111-1111-111111111111"

	// No sources -> no footer.
	if got := sourcesFooterHTML(nil, asker); got != "" {
		t.Fatalf("expected empty html footer, got %q", got)
	}

	sources := []aiAdapter.SourceRef{
		{ContentType: "post", ChannelName: "general", ChannelUUID: "ch1", ContentUUID: "p1"},
		{ContentType: "doc", ContentUUID: "d1"},
		{ContentType: "task"}, // no uuid -> plain text, not a link
	}
	got := sourcesFooterHTML(sources, asker)
	if !strings.HasPrefix(got, "<p>Sources: ") || !strings.HasSuffix(got, "</p>") {
		t.Fatalf("unexpected footer wrapper: %q", got)
	}
	// Channel cites by name and links to the channel.
	if !strings.Contains(got, `<a href="/app/channel/ch1/p1" class="link">#general</a>`) {
		t.Fatalf("expected linked channel citation, got %q", got)
	}
	// Doc links to the doc.
	if !strings.Contains(got, `<a href="/app/doc/d1" class="link">a doc</a>`) {
		t.Fatalf("expected linked doc citation, got %q", got)
	}
	// Task without a uuid stays plain text (no dead link).
	if strings.Contains(got, `href="/app/task`) {
		t.Fatalf("task without uuid should not be linked, got %q", got)
	}
	if !strings.Contains(got, "a task") {
		t.Fatalf("task should still be listed as plain text, got %q", got)
	}

	// Cap at 5 distinct labels (channels are distinct by name).
	many := []aiAdapter.SourceRef{
		{ContentType: "post", ChannelName: "a", ChannelUUID: "1", ContentUUID: "p"},
		{ContentType: "post", ChannelName: "b", ChannelUUID: "2", ContentUUID: "p"},
		{ContentType: "post", ChannelName: "c", ChannelUUID: "3", ContentUUID: "p"},
		{ContentType: "post", ChannelName: "d", ChannelUUID: "4", ContentUUID: "p"},
		{ContentType: "post", ChannelName: "e", ChannelUUID: "5", ContentUUID: "p"},
		{ContentType: "post", ChannelName: "f", ChannelUUID: "6", ContentUUID: "p"},
	}
	if n := strings.Count(sourcesFooterHTML(many, asker), "<a "); n != 5 {
		t.Fatalf("expected the html footer capped at 5 links, got %d", n)
	}
}
