package models

import (
	"testing"

	"github.com/google/uuid"
)

// The toolset parser is shared by the in-app runner and the MCP surface, so both must
// get the same answer from the same column. Two parsers would be two chances to
// disagree about what "enabled" means, and the disagreement would only ever show up as
// an agent doing something nobody granted it.
func TestEnabledToolListParsing(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"normal", `["read_doc","send_message"]`, []string{"read_doc", "send_message"}},
		{"empty array", `[]`, nil},
		{"blank column", ``, nil},
		{"whitespace column", `   `, nil},
		{"malformed json", `{"read_doc":true}`, nil},
		{"truncated json", `["read_doc"`, nil},
		{"entries trimmed", `[" read_doc ","  "]`, []string{"read_doc"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := (&AiAgent{EnabledTools: tc.raw}).EnabledToolList()
			if len(got) != len(tc.want) {
				t.Fatalf("got %#v, want %#v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %#v, want %#v", got, tc.want)
				}
			}
		})
	}
}

// A nil agent must not panic — callers reach this from paths where the row may be
// absent, and the safe reading of "no agent" is "no tools".
func TestEnabledToolListOnNilAgent(t *testing.T) {
	var a *AiAgent
	if got := a.EnabledToolList(); got != nil {
		t.Fatalf("expected nil for a nil agent, got %#v", got)
	}
}

// MALFORMED MUST NOT MEAN UNRESTRICTED. An unparseable column yields an empty list,
// and every caller reads an empty list as "no tools". If it were read as "all tools",
// a corrupted row would silently grant an agent the entire toolset.
func TestMalformedToolsetIsNotAnOpenDoor(t *testing.T) {
	a := &AiAgent{EnabledTools: `not json at all`}
	if len(a.EnabledToolList()) != 0 {
		t.Fatal("a malformed toolset parsed to something non-empty")
	}
}

// The manage rule is shared by the agent builder and the API-token binding path, so
// binding a credential to an agent asks exactly the question editing it asks.
func TestManageableBy(t *testing.T) {
	owner := uuid.New()
	other := uuid.New()
	agent := &AiAgent{CreatedBy: owner}

	if !agent.ManageableBy(owner, false) {
		t.Error("the owner cannot manage their own agent")
	}
	if !agent.ManageableBy(other, true) {
		t.Error("an admin cannot manage another member's agent")
	}
	if agent.ManageableBy(other, false) {
		t.Error("a non-owner non-admin can manage someone else's agent, so they could bind " +
			"their own credential to it and lend it their authority")
	}

	var nilAgent *AiAgent
	if nilAgent.ManageableBy(owner, true) {
		t.Error("a nil agent reported as manageable; an absent row must never authorize")
	}
}
