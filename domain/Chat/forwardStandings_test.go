package domain

import (
	"strings"
	"testing"
)

// A forward's groups and people are read in one query, each distinct id once
// and only as a query variable, so no id from a request reaches the text.
func TestTheForwardStandingsReadIsOneQueryWithIdsAsVariables(t *testing.T) {
	evil := `x")) { uid } all(func: has(user_uuid)) { uid user_email_id } q(func: eq(a, "`
	query, vars := standingsQuery("0x1", []string{"g-1", evil, "g-1", ""}, []string{"u-1", "u-2", "u-1"})
	if strings.Contains(query, "user_email_id") || strings.Contains(query, `g-1`) || strings.Contains(query, "u-1") {
		t.Fatalf("an id reached the query text:\n%s", query)
	}
	want := map[string]string{"$me": "0x1", "$g0": "g-1", "$g1": evil, "$p0": "u-1", "$p1": "u-2"}
	if len(vars) != len(want) {
		t.Fatalf("vars %v, want %v", vars, want)
	}
	for k, v := range want {
		if vars[k] != v {
			t.Errorf("%s = %q, want %q", k, vars[k], v)
		}
		if k != "$me" && !strings.Contains(query, k+": string") {
			t.Errorf("%s isn't declared", k)
		}
	}
	if n := strings.Count(query, "(func:"); n != 4 {
		t.Errorf("%d blocks, want 4 (one per distinct id)", n)
	}
}

func TestTheForwardStandingsAnswerIsFiledById(t *testing.T) {
	got, err := readStandings([]byte(`{
		"g0": [{"uid": "0x5", "dm_grouping_id": "g-1", "dm_is_member": 1}],
		"g1": [],
		"p0": [{"uid": "0x7", "user_uuid": "u-1", "user_name": "cy", "is_external": true, "is_bot": true}],
		"p1": []
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if g := got.Groups["g-1"]; g == nil || g.Uid != "0x5" || g.ParticipantIsMember != 1 {
		t.Errorf("group g-1: %+v", g)
	}
	if p := got.People["u-1"]; p == nil || p.Uid != "0x7" || p.UserName != "cy" || !p.IsExternal || !p.IsBot {
		t.Errorf("person u-1: %+v", p)
	}
	if len(got.Groups) != 1 || len(got.People) != 1 {
		t.Errorf("ids the graph doesn't have were filed: %+v", got)
	}
}
