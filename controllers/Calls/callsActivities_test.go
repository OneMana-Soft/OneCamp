package controllers

import (
	"encoding/json"
	"strings"
	"testing"

	callControlBusiness "github.com/akashc777/OneCamp/business/CallControl"
	callStateBusiness "github.com/akashc777/OneCamp/business/CallState"
	pollBusiness "github.com/akashc777/OneCamp/business/Poll"
)

// The audit log keeps what whoever runs a call's polls and Q&A did, and to
// which poll or question: never a poll's or a question's words, and never
// anyone who voted or asked.
func TestTheAuditKeepsTheActNotThePeople(t *testing.T) {
	pinned := true
	for _, c := range []struct {
		a    callControlBusiness.Activity
		id   string
		want map[string]interface{}
	}{
		{callControlBusiness.Activity{Op: callControlBusiness.OpPoll, Poll: &pollBusiness.CallPoll{Question: "Who's late?", Options: []string{"Ben", "Gia"}, Anonymous: true}}, "p1",
			map[string]interface{}{"session": "s1", "op": "poll", "id": "p1", "anonymous": true, "hide_results": false}},
		{callControlBusiness.Activity{Op: callControlBusiness.OpQuestion, ID: "q1", State: "dismissed", Pinned: &pinned}, "",
			map[string]interface{}{"session": "s1", "op": "question", "id": "q1", "state": "dismissed", "pinned": true}},
		{callControlBusiness.Activity{Op: callControlBusiness.OpQA, QA: &callStateBusiness.QA{Open: true, Shown: "answered", Anonymous: true}}, "",
			map[string]interface{}{"session": "s1", "op": "qa", "open": true, "shown": "answered", "anonymous": true}},
	} {
		got := activityAudit("s1", c.id, c.a)
		raw, _ := json.Marshal(got)
		for _, words := range []string{"Who's late?", "Ben", "Gia"} {
			if strings.Contains(string(raw), words) {
				t.Errorf("%s's audit keeps %q: %s", c.a.Op, words, raw)
			}
		}
		if len(got) != len(c.want) {
			t.Errorf("%s's audit: %v, want %v", c.a.Op, got, c.want)
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s's audit %s: %v, want %v", c.a.Op, k, got[k], v)
			}
		}
	}
}
