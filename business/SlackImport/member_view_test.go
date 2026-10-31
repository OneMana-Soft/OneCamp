package business

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A member's own read of their Slack DMs is shown whole to that member alone:
// anyone else sees that it happened and how far it got, never the names it
// noted or why it failed. Every other job is shown whole to everyone.
func TestAMembersReadIsShownWholeToThemAlone(t *testing.T) {
	member, admin := uuid.New(), uuid.New()
	read := json.RawMessage(`{"member_dms": true, "skip_subtypes": false}`)
	team := json.RawMessage(`{"skip_subtypes": false}`)

	if !MemberRead(read) || MemberRead(team) || MemberRead(nil) || MemberRead(json.RawMessage(`not json`)) {
		t.Errorf("MemberRead: read %v, team %v, none %v, garbled %v", MemberRead(read), MemberRead(team), MemberRead(nil), MemberRead(json.RawMessage(`not json`)))
	}
	for _, c := range []struct {
		name    string
		options json.RawMessage
		by      *uuid.UUID
		viewer  uuid.UUID
		shown   bool
	}{
		{"the member's read, to them", read, &member, member, true},
		{"the member's read, to an admin", read, &member, admin, false},
		{"a read nobody started, to anyone", read, nil, admin, false},
		{"the team's import, to an admin", team, &admin, admin, true},
		{"the team's import, to another admin", team, &member, admin, true},
	} {
		if got := ContentsShownTo(c.options, c.by, c.viewer); got != c.shown {
			t.Errorf("%s: shown %v, want %v", c.name, got, c.shown)
		}
	}

	why := "Reading from Slack failed: #hiring-plans was too large"
	if got := HiddenFailure(&why); got == nil || strings.Contains(*got, "hiring") || !strings.Contains(*got, "didn't finish") {
		t.Errorf("the failure as others read it: %v", got)
	}
	if HiddenFailure(nil) != nil {
		t.Error("no failure became one")
	}

	progress := json.RawMessage(`{
		"read": {"team_id": "T1", "user_id": "U2", "users": 4, "groups": 1, "dms": 2, "mpims": 1, "conversations": 4, "of": 4, "messages": 31, "replies": 3, "files": 2, "bytes": 2048},
		"channels": {"total": 1, "created": 1, "renamed": ["hiring-plans → hiring-plans-2"], "into_general": ""},
		"users": {"total": 4, "reused": 4},
		"slack_team": "T1"
	}`)
	shown := string(HiddenProgress(progress))
	for _, hidden := range []string{"hiring", "renamed", "channels", "T1", "U2"} {
		if strings.Contains(shown, hidden) {
			t.Errorf("the progress others see holds %q: %s", hidden, shown)
		}
	}
	var counts struct {
		Read struct {
			DMs, MPIMs, Groups, Conversations, Of, Messages int
		} `json:"read"`
	}
	if err := json.Unmarshal([]byte(shown), &counts); err != nil || counts.Read.DMs != 2 || counts.Read.MPIMs != 1 ||
		counts.Read.Groups != 1 || counts.Read.Conversations != 4 || counts.Read.Of != 4 || counts.Read.Messages != 31 {
		t.Errorf("the progress others see lost its counts: %s (%v)", shown, err)
	}
	if got := string(HiddenProgress(json.RawMessage(`{"channels": {"renamed": ["x"]}}`))); got != `{}` {
		t.Errorf("a progress with no read shows %s", got)
	}
	if HiddenProgress(nil) != nil || HiddenProgress(json.RawMessage(`[`)) != nil {
		t.Error("no progress, or a garbled one, became one")
	}
}
