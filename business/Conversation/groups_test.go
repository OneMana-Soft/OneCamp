package business

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestEachShareChoiceStartsWhereItSays(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		share Share
		since time.Time
		ok    bool
	}{
		{ShareNone, time.Time{}, false},
		{ShareDay, now.Add(-24 * time.Hour), true},
		{ShareWeek, now.Add(-7 * 24 * time.Hour), true},
		{ShareAll, time.Time{}, true},
		{"everything-and-more", time.Time{}, false},
	} {
		since, ok := c.share.since(now)
		if ok != c.ok || !since.Equal(c.since) {
			t.Errorf("%q: since %v %v, want %v %v", c.share, since, ok, c.since, c.ok)
		}
	}
	if Share("everything-and-more").valid() || !ShareNone.valid() {
		t.Error("the choices the sheet offers aren't the valid ones")
	}
}

func TestAPrivateChannelsNameFollowsTheOneNameRule(t *testing.T) {
	if got, err := privateChannelName("  launch-plan "); err != nil || got != "launch-plan" {
		t.Errorf("a good name: %q %v", got, err)
	}
	for _, bad := range []string{"", "x", "bad/name", strings.Repeat("a", 41)} {
		_, err := privateChannelName(bad)
		var rj *sendBusiness.Rejection
		if !errors.As(err, &rj) || rj.Status != http.StatusBadRequest || rj.Msg != helpers.NameRuleMessage {
			t.Errorf("%q: %v, want the name rule's words", bad, err)
		}
	}
}

func TestAGroupsNameIsTidiedAndBounded(t *testing.T) {
	if got, err := cleanGroupName("  Launch \t crew  "); err != nil || got != "Launch crew" {
		t.Errorf("tidied: %q %v", got, err)
	}
	if got, err := cleanGroupName(""); err != nil || got != "" {
		t.Errorf("an empty name takes the name away: %q %v", got, err)
	}
	long := strings.Repeat("é", maxGroupName+1)
	if _, err := cleanGroupName(long); !isRefusal(err, http.StatusBadRequest) {
		t.Errorf("a name past the limit: %v", err)
	}
	if got, err := cleanGroupName(strings.Repeat("é", maxGroupName)); err != nil || got == "" {
		t.Errorf("a name at the limit, in characters not bytes: %v", err)
	}
	if _, err := cleanGroupName("a\u0007b"); err == nil {
		t.Error("a control character was kept")
	}
}

func isRefusal(err error, status int) bool {
	var rj *sendBusiness.Rejection
	return errors.As(err, &rj) && rj.Status == status
}

func TestALineNamesPeopleAsASentenceDoes(t *testing.T) {
	p := func(n string) *dgraphStruct.DgraphUser { return &dgraphStruct.DgraphUser{UserName: n} }
	for _, c := range []struct {
		people []*dgraphStruct.DgraphUser
		want   string
	}{
		{nil, "someone"},
		{[]*dgraphStruct.DgraphUser{p("Priya")}, "Priya"},
		{[]*dgraphStruct.DgraphUser{p("Priya"), p("Jonas")}, "Priya and Jonas"},
		{[]*dgraphStruct.DgraphUser{p("Priya"), p("Jonas"), p("Sam")}, "Priya, Jonas and Sam"},
	} {
		if got := names(c.people); got != c.want {
			t.Errorf("%d people: %q, want %q", len(c.people), got, c.want)
		}
	}
	if got := lineHTML(`Maya <b>added</b> "Priya"`); got != "<p>Maya &lt;b&gt;added&lt;/b&gt; &#34;Priya&#34;</p>" {
		t.Errorf("a line's words are text, never markup: %q", got)
	}
}

// A copied poll keeps its question and loses its live block: its votes are
// the first conversation's, which the people added can't open.
func TestACopiedPollKeepsItsQuestionOnly(t *testing.T) {
	poll := `<p><strong>Poll:</strong> Friday or Monday?</p><div data-type="poll" data-id="5b0e7c1a-8d4f-4d3b-9b8f-1a2b3c4d5e6f"></div>`
	got := copiedHTML(poll)
	if strings.Contains(got, "data-type") || !strings.Contains(got, "Friday or Monday?") {
		t.Errorf("copied poll: %q", got)
	}
	if plain := "<p>No poll here</p>"; copiedHTML(plain) != plain {
		t.Errorf("a message without a poll changed: %q", copiedHTML(plain))
	}
}

func TestSharedTextCountsWhatWasShared(t *testing.T) {
	if got := sharedText(1); got != "1 earlier message was shared from the conversation before." {
		t.Errorf("one: %q", got)
	}
	if got := sharedText(40); !strings.HasPrefix(got, "40 earlier messages and replies") {
		t.Errorf("forty: %q", got)
	}
}
