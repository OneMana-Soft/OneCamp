package business

import (
	"reflect"
	"testing"

	"github.com/akashc777/OneCamp/business/Access/level"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

// A change's shifts reach the audit log in its level names, none as "".
func TestAccessShiftsNameLevelsAsTheAuditLogDoes(t *testing.T) {
	got := accessShifts(docBusiness.Explained{People: []docBusiness.Shift{
		{UserUUID: "lee", From: level.View, To: level.None},
		{UserUUID: "kim", From: level.Full, To: level.Comment},
	}})
	want := []auditBusiness.AccessShift{{UserUUID: "lee", From: "view", To: ""}, {UserUUID: "kim", From: "full", To: "comment"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("accessShifts = %+v, want %+v", got, want)
	}
}

// The row says which agent made the change and how many pages under the doc
// it reached, and nothing when neither applies.
func TestTakingMetaNamesTheAgentAndTheSubpages(t *testing.T) {
	if got := takingMeta(Subject{UserUUID: "maya"}, []string{"d"}); len(got) != 0 {
		t.Fatalf("a person's change to one page = %v, want nothing more", got)
	}
	got := takingMeta(Subject{UserUUID: "maya", AgentUUID: "a-1"}, []string{"d", "s1", "s2"})
	if want := map[string]interface{}{"agent_uuid": "a-1", "subpages": 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("an agent's change to a page and 2 subpages = %v, want %v", got, want)
	}
}
