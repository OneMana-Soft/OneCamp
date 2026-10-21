package business

import (
	"context"
	"errors"
	"strings"
	"testing"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
)

// A built-in's card is pressed the way an app's is: through business/Cards,
// which checks the press against the card the server sealed, and only then
// hands it to the command's InteractionHandler. The old route runs nothing.

func shuffleCard() []commandAdapter.Block {
	return []commandAdapter.Block{
		{Type: "section", Text: &commandAdapter.BlockText{Type: "plain_text", Text: "A cat"}},
		{Type: "actions", Elements: []commandAdapter.BlockElement{
			{Type: "button", Text: &commandAdapter.BlockText{Type: "plain_text", Text: "Shuffle"}, ActionID: "shuffle"},
		}},
	}
}

func registerTestBuiltin(t *testing.T, name string, pressed *[]Press) {
	t.Helper()
	old, had := interactionRegistry[name]
	t.Cleanup(func() {
		if had {
			interactionRegistry[name] = old
		} else {
			delete(interactionRegistry, name)
		}
	})
	RegisterInteraction(name, func(_ context.Context, cc CommandContext, p Press) (*commandAdapter.CommandResponse, error) {
		*pressed = append(*pressed, p)
		if cc.User.UserDgraphInfo.Uuid != "sam" {
			t.Errorf("the press reached the handler as %q", cc.User.UserDgraphInfo.Uuid)
		}
		return &commandAdapter.CommandResponse{ResponseType: "ephemeral", Ephemeral: true, Blocks: shuffleCard(), ReplaceOriginal: true}, nil
	})
}

func sealedBuiltinAnswer(t *testing.T, name string) string {
	t.Helper()
	resp := &commandAdapter.CommandResponse{Blocks: shuffleCard(), TriggerID: "trig-9"}
	sealAnswer(resp, cardsBusiness.Owner{Kind: cardsBusiness.OwnerBuiltin, ID: name}, "sam")
	if resp.CardToken == "" {
		t.Fatal("a built-in's answer with a button wasn't sealed")
	}
	return resp.CardToken
}

func TestABuiltInsCardIsPressedThroughTheCheckAndAnsweredSealedAgain(t *testing.T) {
	t.Setenv("JWT_SECRET", "builtin-presses-test-secret")
	var pressed []Press
	registerTestBuiltin(t, "test-shuffle", &pressed)
	token := sealedBuiltinAnswer(t, "test-shuffle")

	reply, err := cardsBusiness.Act(context.Background(), cardsBusiness.Who{UserUUID: "sam"}, token, "shuffle", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(pressed) != 1 || pressed[0].TriggerID != "trig-9" || pressed[0].ActionID != "shuffle" {
		t.Fatalf("the handler heard %+v", pressed)
	}
	// The card it answers with still has a button, so it is sealed again,
	// for the same person, and keeps the invocation's trigger.
	if reply == nil || reply.CardToken == "" || reply.TriggerID != "trig-9" || !reply.ReplaceOriginal {
		t.Fatalf("the reply: %+v", reply)
	}
	if _, err := cardsBusiness.Act(context.Background(), cardsBusiness.Who{UserUUID: "sam"}, reply.CardToken, "shuffle", ""); err != nil {
		t.Fatalf("pressing the new card: %v", err)
	}
}

func TestABuiltInsCardRefusesSomeoneElseAndAnActionItHasNot(t *testing.T) {
	t.Setenv("JWT_SECRET", "builtin-presses-test-secret")
	var pressed []Press
	registerTestBuiltin(t, "test-shuffle", &pressed)
	token := sealedBuiltinAnswer(t, "test-shuffle")

	if _, err := cardsBusiness.Act(context.Background(), cardsBusiness.Who{UserUUID: "ana"}, token, "shuffle", ""); !errors.Is(err, cardsBusiness.ErrNotYours) {
		t.Fatalf("someone else pressing: %v", err)
	}
	if _, err := cardsBusiness.Act(context.Background(), cardsBusiness.Who{UserUUID: "sam"}, token, "send", ""); !errors.Is(err, cardsBusiness.ErrNoAction) {
		t.Fatalf("an action the card hasn't: %v", err)
	}
	if len(pressed) != 0 {
		t.Fatalf("a refused press reached the handler: %+v", pressed)
	}
}

func TestACardWhoseCommandHasNoHandlerHasNoOwner(t *testing.T) {
	t.Setenv("JWT_SECRET", "builtin-presses-test-secret")
	token := sealedBuiltinAnswer(t, "test-gone")
	if _, err := cardsBusiness.Act(context.Background(), cardsBusiness.Who{UserUUID: "sam"}, token, "shuffle", ""); !errors.Is(err, cardsBusiness.ErrNoOwner) {
		t.Fatalf("a press on a card nothing answers: %v", err)
	}
}

func TestTheOldInteractRouteRunsNothingAndSaysHowToGetAFreshCard(t *testing.T) {
	var pressed []Press
	registerTestBuiltin(t, "test-shuffle", &pressed)
	resp := HandleInteract("/Test-Shuffle")
	if len(pressed) != 0 {
		t.Fatalf("the old route ran the handler: %+v", pressed)
	}
	if resp == nil || !strings.Contains(resp.Text, "out of date") || !strings.Contains(resp.Text, "/test-shuffle") {
		t.Fatalf("the old route answered %+v", resp)
	}
}
