package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// Search in one conversation, from its header (QA F-15): the filter keeps a
// channel's messages, replies and files, or a DM's or a group's, and the
// text must still match. Without it, every search stays as it was.
func TestWithinConversation(t *testing.T) {
	if got := withinConversation("", ""); got != "" {
		t.Fatalf("no conversation: %q, want nothing", got)
	}
	for _, c := range []struct {
		name, channel, grouping string
		fields                  []string
		id                      string
	}{
		{"a channel", "c-1", "", []string{"post_ch_id", "comment_channel_id", "attachment_channel_id"}, "c-1"},
		{"a DM or a group", "", "a b", []string{"chat_grp_id.keyword", "comment_chat_grp_id", "attachment_chat_grp_id"}, "a b"},
	} {
		got := withinConversation(c.channel, c.grouping)
		// As it sits in the query: after the should list, in its bool.
		query := `{ "bool": { "should": [ { "match_all": {} } ]` + got + ` } }`
		if !json.Valid([]byte(query)) {
			t.Fatalf("%s: not JSON in place: %s", c.name, query)
		}
		var parsed struct {
			Bool struct {
				MinimumShouldMatch int `json:"minimum_should_match"`
				Filter             []struct {
					Bool struct {
						Should []map[string]map[string]string `json:"should"`
					} `json:"bool"`
				} `json:"filter"`
			} `json:"bool"`
		}
		if err := json.Unmarshal([]byte(query), &parsed); err != nil {
			t.Fatal(err)
		}
		if parsed.Bool.MinimumShouldMatch != 1 {
			t.Errorf("%s: the words needn't match: %s", c.name, got)
		}
		if len(parsed.Bool.Filter) != 1 || len(parsed.Bool.Filter[0].Bool.Should) != len(c.fields) {
			t.Fatalf("%s: %s", c.name, got)
		}
		for i, term := range parsed.Bool.Filter[0].Bool.Should {
			if term["term"][c.fields[i]] != c.id {
				t.Errorf("%s: clause %d is %v, want %s = %q", c.name, i, term, c.fields[i], c.id)
			}
		}
	}
	// An id is a JSON string, whatever it holds.
	if got := withinConversation(`x" } }, { "match_all": {`, ""); !strings.Contains(got, `x\" } }, { \"match_all\": {`) {
		t.Errorf("an id with quotes in it wasn't escaped: %s", got)
	}
}
