package notification

import (
	"slices"
	"testing"
)

// Who hears of a reply in a thread: of the post's author, those who replied
// before and those mentioned, the ones in the conversation now, by their setting.
// Each setting against each part a person can have in the thread.
func TestWhoHearsOfAThreadReply(t *testing.T) {
	type part int
	const (
		author part = iota
		replied
		mentioned
		bystander // in the channel, not in the thread
	)
	for _, tc := range []struct {
		level string // "" is someone not in the channel
		part  part
		told  bool
	}{
		// "all" and "mention" ("Mentions and threads you're in") hear of their
		// threads: the default heard nothing addressed to it (TN-BUG-1), and
		// "mention" nothing of threads it had replied in (TN-BUG-15).
		{"all", author, true}, {"all", replied, true}, {"all", mentioned, true}, {"all", bystander, false},
		{"mention", author, true}, {"mention", replied, true}, {"mention", mentioned, true}, {"mention", bystander, false},
		// "block" ("Nothing") hears only what is addressed to it.
		{"block", author, true}, {"block", replied, false}, {"block", mentioned, true}, {"block", bystander, false},
		// Nobody outside the conversation, whatever they were to the thread
		// (TN-BUG-2): mentioned from outside it, or removed from it since.
		{"", author, false}, {"", replied, false}, {"", mentioned, false},
	} {
		levels := map[string]string{"x": tc.level, "max": "all"}
		if tc.level == "" {
			delete(levels, "x")
		}
		var postBy string
		var repliers, named []string
		switch tc.part {
		case author:
			postBy = "x"
		case replied:
			postBy, repliers = "bo", []string{"x"}
		case mentioned:
			postBy, named = "bo", []string{"x"}
		case bystander:
			postBy = "bo"
		}
		got := ThreadAudience(levels, "max", postBy, repliers, named, Follows{})
		if told := slices.Contains(got.Told, "x"); told != tc.told {
			t.Errorf("level %q, part %d: told = %v, want %v", tc.level, tc.part, told, tc.told)
		}
	}
}

// The live Activity items go to the author ("replied to your message") and to
// those mentioned, from the same audience; each person is told once.
func TestAThreadReplysAudienceIsOneList(t *testing.T) {
	levels := map[string]string{"bo": "all", "ada": "all", "cy": "all", "dee": "mention", "eve": "block", "gus": "block", "max": "all", "zed": "all"}
	got := ThreadAudience(levels, "max", "bo",
		[]string{"ada", "dee", "gus", "fay", "max", "ada"},
		[]string{"cy", "eve", "jonas", "bo"}, Follows{})
	if want := []string{"bo", "ada", "dee", "cy", "eve"}; !slices.Equal(got.Told, want) {
		t.Errorf("told %v, want %v", got.Told, want)
	}
	if got.Author != "bo" {
		t.Errorf("the author told as %q", got.Author)
	}
	if want := []string{"cy", "eve"}; !slices.Equal(got.Mentioned, want) {
		t.Errorf("the live mention item to %v, want %v (the author gets the reply item instead)", got.Mentioned, want)
	}
	// The author's own reply in their thread tells them nothing.
	if got := ThreadAudience(map[string]string{"bo": "all", "ada": "all"}, "bo", "bo", []string{"ada"}, nil, Follows{}); !slices.Equal(got.Told, []string{"ada"}) || got.Author != "" {
		t.Errorf("the author's own reply: told %v, author %q", got.Told, got.Author)
	}
	// Ids are matched whatever their case: the graph and Postgres write them
	// their own ways.
	if got := ThreadAudience(map[string]string{"abc": "all"}, "max", "ABC", nil, nil, Follows{}); !slices.Equal(got.Told, []string{"ABC"}) {
		t.Errorf("an id in another case: told %v", got.Told)
	}
	// Nobody in the channel: nobody told.
	if got := ThreadAudience(nil, "max", "bo", []string{"ada"}, []string{"cy"}, Follows{}); len(got.Told) != 0 || got.Author != "" || len(got.Mentioned) != 0 {
		t.Errorf("with no members: %+v", got)
	}
}

// Following a thread by hand (chat P2) makes someone one of its people: they
// hear of its replies as a replier does, by their setting. Turning it off
// takes them out of it, whatever their part, but a reply that mentions them by
// name still reaches them, as Slack's does.
func TestFollowingAThreadDecidesWhoHears(t *testing.T) {
	for _, tc := range []struct {
		name    string
		level   string
		part    string // "author", "replied", "mentioned" or "" for none
		follows Follows
		told    bool
	}{
		{"followed by hand on all", "all", "", Follows{On: []string{"x"}}, true},
		{"followed by hand on mentions and threads", "mention", "", Follows{On: []string{"x"}}, true},
		{"followed by hand on nothing", "block", "", Follows{On: []string{"x"}}, false},
		{"followed by hand from outside the conversation", "", "", Follows{On: []string{"x"}}, false},
		{"their own thread turned off", "all", "author", Follows{Off: []string{"x"}}, false},
		{"a thread they replied in turned off", "all", "replied", Follows{Off: []string{"x"}}, false},
		{"turned off, but mentioned by this reply", "all", "mentioned", Follows{Off: []string{"x"}}, true},
		{"turned off, ids in another case", "all", "replied", Follows{Off: []string{"X"}}, false},
	} {
		levels := map[string]string{"x": tc.level, "max": "all", "bo": "all"}
		if tc.level == "" {
			delete(levels, "x")
		}
		postBy := "bo"
		var repliers, named []string
		switch tc.part {
		case "author":
			postBy = "x"
		case "replied":
			repliers = []string{"x"}
		case "mentioned":
			named = []string{"x"}
		}
		got := ThreadAudience(levels, "max", postBy, repliers, named, tc.follows)
		if told := slices.Contains(got.Told, "x"); told != tc.told {
			t.Errorf("%s: told = %v, want %v (told %v)", tc.name, told, tc.told, got.Told)
		}
		if tc.part == "author" && !tc.told && got.Author != "" {
			t.Errorf("%s: the author's reply item still goes to %q", tc.name, got.Author)
		}
	}
	// A follower comes after those who replied, before those mentioned, and
	// is told once however many lists they are in.
	levels := map[string]string{"bo": "all", "ada": "all", "cy": "all", "fay": "all", "max": "all"}
	got := ThreadAudience(levels, "max", "bo", []string{"ada"}, []string{"cy"}, Follows{On: []string{"fay", "ada", "max"}})
	if want := []string{"bo", "ada", "fay", "cy"}; !slices.Equal(got.Told, want) {
		t.Errorf("told %v, want %v: never the writer, even following", got.Told, want)
	}
}
