package business

import (
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)

func valid() SaveInput {
	return SaveInput{ItemType: "post", ItemID: "b1d0", Link: "/app/channel/abc-123/def-456", Title: "Ship it"}
}

func TestLinksMustStayInsideTheApp(t *testing.T) {
	for _, link := range []string{
		"https://evil.example/app/x",
		"//evil.example/app/x",
		"/app/../admin",
		"/app/channel/x#frag",
		"/app/channel/x?next=https://evil.example",
		"javascript:alert(1)",
		"/api/later",
		"/app/",
		"/app/x y",
	} {
		in := valid()
		in.Link = link
		if in.Validate(now) == nil {
			t.Errorf("link %q was accepted", link)
		}
	}
	for _, link := range []string{
		"/app/channel/abc-123",
		"/app/task/0e6c-11",
		"/app/doc/7f?comment=abc-1",
		"/app/chat/group/abc_def",
	} {
		in := valid()
		in.Link = link
		if err := in.Validate(now); err != nil {
			t.Errorf("link %q refused: %v", link, err)
		}
	}
}

func TestOnlyKnownThingsCanBeSaved(t *testing.T) {
	in := valid()
	in.ItemType = "user"
	if in.Validate(now) == nil {
		t.Fatal("an unknown item type was accepted")
	}
	in = valid()
	in.ItemID = " "
	if in.Validate(now) == nil {
		t.Fatal("an empty item id was accepted")
	}
}

func TestReminderWindow(t *testing.T) {
	at := func(d time.Duration) *time.Time { x := now.Add(d); return &x }
	if ValidateRemindAt(nil, now) != nil {
		t.Fatal("no reminder must be fine")
	}
	if ValidateRemindAt(at(-time.Minute), now) != nil {
		t.Fatal("a reminder a minute ago (slow click on 'now') must be fine")
	}
	if ValidateRemindAt(at(-time.Hour), now) == nil {
		t.Fatal("a reminder an hour in the past was accepted")
	}
	if ValidateRemindAt(at(400*24*time.Hour), now) == nil {
		t.Fatal("a reminder more than a year away was accepted")
	}
	if ValidateRemindAt(at(30*24*time.Hour), now) != nil {
		t.Fatal("a reminder next month was refused")
	}
}

func TestTitleIsOneTidyLine(t *testing.T) {
	in := valid()
	in.Title = "  Line one\n\nline   two\t" + strings.Repeat("é", 400)
	if err := in.Validate(now); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(in.Title, "\n\t") || !strings.HasPrefix(in.Title, "Line one line two é") {
		t.Fatalf("title not tidied: %q", in.Title[:30])
	}
	if n := len([]rune(in.Title)); n != maxTitle {
		t.Fatalf("title is %d runes, want %d", n, maxTitle)
	}
	if !strings.HasSuffix(in.Title, "…") {
		t.Fatal("a clipped title should say so")
	}
}
