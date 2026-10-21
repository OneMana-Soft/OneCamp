package bodyrefs

import (
	"reflect"
	"testing"
)

const (
	d1 = "11111111-1111-4111-8111-111111111111"
	t1 = "22222222-2222-4222-8222-222222222222"
	p1 = "33333333-3333-4333-8333-333333333333"
	u1 = "44444444-4444-4444-8444-444444444444"
)

// TestScanReadsEveryFormTheAppWrites: chips, links (any host, link chips),
// links written out in text, @mentions; each once; board and channel chips and
// a to-do's task id are not links.
func TestScanReadsEveryFormTheAppWrites(t *testing.T) {
	body := `<p>See <span data-type="entityMention" data-id="doc:` + d1 + `" data-label="Plan"></span>,
<a href="https://onecamp.example/app/task/` + t1 + `?x=1">the task</a>,
<a data-type="link-mention" href="/app/project/` + p1 + `/board">the board</a>,
<span data-type="entityMention" data-id="board:` + d1 + `"></span>
<span data-type="entityMention" data-id="channel:general"></span>
<span data-type="mention" data-id="` + u1 + `@0x2">@Maya</span>
<li data-type="taskItem" data-task-id="` + t1 + `"></li>
again <span data-type="entityMention" data-id="doc:` + d1 + `"></span></p>`
	got := Scan(body)
	want := []Link{{KindDoc, d1, false}, {KindTask, t1, false}, {KindProject, p1, false}}
	if !reflect.DeepEqual(got.Links, want) {
		t.Errorf("links %+v, want %+v", got.Links, want)
	}
	if !reflect.DeepEqual(got.People, []string{u1}) {
		t.Errorf("people %v", got.People)
	}
}

// TestALinkInTextIsMarkedAndMarkupWins: a link written out in Markdown or
// plain text counts, marked FromText; the same link as a chip too is markup.
func TestALinkInTextIsMarkedAndMarkupWins(t *testing.T) {
	got := Scan("Have a look: [plan](https://x.test/app/doc/" + d1 + ") and https://x.test/app/task/" + t1)
	want := []Link{{KindDoc, d1, true}, {KindTask, t1, true}}
	if !reflect.DeepEqual(got.Links, want) {
		t.Errorf("text links %+v, want %+v", got.Links, want)
	}
	got = Scan("/app/doc/" + d1 + ` <a href="/app/doc/` + d1 + `">x</a>`)
	if len(got.Links) != 1 || got.Links[0].FromText {
		t.Errorf("markup should win over text: %+v", got.Links)
	}
	if ids := got.Of(KindDoc, true); len(ids) != 1 {
		t.Errorf("Of(markupOnly) %v", ids)
	}
	if ids := Scan("/app/doc/"+d1).Of(KindDoc, true); len(ids) != 0 {
		t.Errorf("a text link counted as markup: %v", ids)
	}
}

func TestAppLinkTakesOnlyUUIDs(t *testing.T) {
	for href, ok := range map[string]bool{
		"/app/doc/" + d1:                    true,
		"https://h/app/project/" + p1 + "/": true,
		"/app/doc/not-a-uuid":               false,
		"/app/channel/" + d1:                false,
		"/doc/" + d1:                        false,
	} {
		if _, _, got := AppLink(href); got != ok {
			t.Errorf("AppLink(%q) = %v, want %v", href, got, ok)
		}
	}
}
