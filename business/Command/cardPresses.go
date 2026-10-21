package business

import (
	"context"

	cardsBusiness "github.com/akashc777/OneCamp/business/Cards"
	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A press on an app's card reaches the app here, once business/Cards has
// checked it: signed as its commands are, with the press's trigger (to open a
// modal) and a response URL for its answer. Before, any press a client
// described was forwarded, unchecked, from POST /command/interact.
func init() {
	cardsBusiness.RegisterOwner(cardsBusiness.OwnerApp, routeAppPress)
	cardsBusiness.RegisterOwner(cardsBusiness.OwnerBuiltin, routeBuiltinPress)
}

// findApp and sendPress are seams for tests.
var (
	findApp   = slashModel.GetAppByID
	sendPress = func(handlerURL, secret string, payload map[string]interface{}, cc CommandContext, appID string) {
		resp := postSigned(appCtx, handlerURL, secret, payload)
		if resp == nil {
			return
		}
		resp.ReplaceOriginal = true
		if resp.TriggerID == "" {
			resp.TriggerID = cc.TriggerID
		}
		sealAnswer(resp, appOwner(appID), cc.User.UserDgraphInfo.Uuid)
		deliverAsync(appCtx, cc, resp)
	}
)

func routeAppPress(ctx context.Context, a cardsBusiness.Action) (*cardsBusiness.Reply, error) {
	appID, err := uuid.Parse(a.Owner.ID)
	if err != nil {
		return nil, cardsBusiness.ErrNoOwner
	}
	app, err := findApp(ctx, appID)
	if err != nil {
		return nil, err
	}
	if app == nil || !app.IsEnabled || app.HandlerUrl == nil || *app.HandlerUrl == "" {
		return nil, cardsBusiness.ErrNoOwner
	}
	if err := forwardCheck(*app.HandlerUrl); err != nil {
		return nil, cardsBusiness.ErrNoOwner
	}
	var u userModels.UserInfo
	u.UserDgraphInfo.Uuid = a.Who.UserUUID
	// The card a press came from was shown with its trigger: the answer replaces it.
	cc := CommandContext{User: u, TriggerID: a.Ref.ID}
	payload := map[string]interface{}{
		"type":       "block_actions",
		"action_id":  a.ActionID,
		"value":      a.Value,
		"user_id":    a.Who.UserUUID,
		"trigger_id": a.TriggerID,
		"card":       map[string]string{"host": a.Ref.Host, "id": a.Ref.ID},
	}
	if url := newResponseURL(ctx, appID, app.Name, deref(app.IconUrl), cc); url != "" {
		payload["response_url"] = url
	}
	secret := appSigningSecret(app)
	helpers.GoSafeNamed("Command.sendPress", func() { sendPress(*app.HandlerUrl, secret, payload, cc, appID.String()) })
	return nil, nil // the app answers over MQTT
}

// routeBuiltinPress takes a checked press on a built-in's card to that
// command's InteractionHandler, and answers at once: the card to show
// instead (sealed again, when it still has something to press), a note, or a
// client action. The card's id is the trigger of the invocation it answered,
// which is how a built-in finds what it kept (giphy's results).
func routeBuiltinPress(ctx context.Context, a cardsBusiness.Action) (*cardsBusiness.Reply, error) {
	handler, ok := interactionRegistry[a.Owner.ID]
	if !ok {
		return nil, cardsBusiness.ErrNoOwner
	}
	var u userModels.UserInfo
	u.UserDgraphInfo.Uuid = a.Who.UserUUID
	cc := CommandContext{User: u, Command: a.Owner.ID, TriggerID: a.Ref.ID}
	resp, err := handler(ctx, cc, Press{TriggerID: a.Ref.ID, ActionID: a.ActionID, Value: a.Value})
	if err != nil {
		logErr(ctx, "routeBuiltinPress/"+a.Owner.ID, err)
		return errorResponse("That action failed. Try again in a moment."), nil
	}
	if resp == nil {
		return nil, nil
	}
	if resp.TriggerID == "" {
		resp.TriggerID = a.Ref.ID
	}
	sealAnswer(resp, a.Owner, a.Who.UserUUID)
	return resp, nil
}
