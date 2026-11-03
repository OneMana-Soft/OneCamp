package business

import (
	"errors"
	"net/http"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// V-126: a DM's call is with a person. An agent (a bot, by class) takes
// messages, not calls, so the token route refuses one before any room or
// token, in words the call page shows, whatever a client offers.
func TestADMCallIsRefusedWithAnAgentOnly(t *testing.T) {
	err := RefuseCallWith(&dgraphStruct.DgraphUser{Uuid: "ai", IsBot: true})
	var refusal *helpers.CallRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("a call with an agent: got %v, want a CallRefusal", err)
	}
	if refusal.Code != "agent" || refusal.Status != http.StatusForbidden || refusal.Msg == "" {
		t.Fatalf("a call with an agent: got %+v, want code agent, 403 and words", refusal)
	}
	for _, p := range []*dgraphStruct.DgraphUser{{Uuid: "maya"}, nil} {
		if err := RefuseCallWith(p); err != nil {
			t.Fatalf("a call with %+v: got %v, want none", p, err)
		}
	}
}

// CR-178's F1: a group's call is asked by everyone else in it. Only agents is
// refused in the same words; a person among them, an external guest included,
// is someone to call; nobody else is answered by the routes first.
func TestAGroupCallIsRefusedWhenEveryoneElseIsAnAgent(t *testing.T) {
	ai, nova := &dgraphStruct.DgraphUser{Uuid: "ai", IsBot: true}, &dgraphStruct.DgraphUser{Uuid: "nova", IsBot: true}
	maya, guest := &dgraphStruct.DgraphUser{Uuid: "maya"}, &dgraphStruct.DgraphUser{Uuid: "guest", IsExternal: true}
	for _, others := range [][]*dgraphStruct.DgraphUser{{ai, nova}, {nil, ai}} {
		if err := RefuseCallWith(others...); !errors.Is(err, errCallWithAgent) {
			t.Fatalf("a call with only agents %+v: got %v, want the agent refusal", others, err)
		}
	}
	for _, others := range [][]*dgraphStruct.DgraphUser{{ai, maya}, {guest, ai}, {maya}, {}, {nil}} {
		if err := RefuseCallWith(others...); err != nil {
			t.Fatalf("a call with %+v: got %v, want none", others, err)
		}
	}
}
