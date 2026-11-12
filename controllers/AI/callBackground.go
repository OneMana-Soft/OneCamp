package controllers

// POST /ai/call-background (meet-09): a member describes a background for
// their camera and their workspace's model answers with a recipe (colours, a
// built-in layout, extras), which their browser paints and keeps on their
// device (business/AI callBackground.go).
//
// Who may: any signed-in member whose workspace AI is on, through the model
// ResolveUserModel picks for them, under the same gate and rate limit as doc
// AI. The words go to that model only: they are not stored, and no log line
// here carries them or the model's answer. The answer is the recipe and the
// model's name; nothing is sent to anyone else.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	business "github.com/akashc777/OneCamp/business/AI"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// callBackgroundDeadline ends the model's work well before Cloudflare's 100 s
// turns a slow answer into a failed request.
var callBackgroundDeadline = 60 * time.Second

// callBackgroundFailed is what a person reads when the model failed, timed out
// or wrote something that isn't a recipe.
const callBackgroundFailed = "Couldn't make one just now."

// CallBackgroundRequest is the person's description of the background.
type CallBackgroundRequest struct {
	Words string `json:"words"`
}

// MakeCallBackground handles POST /ai/call-background:
// {"msg":"ok","data":{"recipe":{...},"model":{...}}}. A model failure or an
// unreadable answer is a 503 (never a 502, which Cloudflare shows as a
// network error); "AI is switched off" is a 503 told apart by its words.
func MakeCallBackground(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req CallBackgroundRequest
	userInfo, ok := gateAI(w, r, ctx, &req)
	if !ok {
		return
	}
	words := strings.TrimSpace(req.Words)
	switch {
	case words == "":
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Describe the background you want."})
		return
	case utf8.RuneCountInString(words) > business.MaxCallSceneWords:
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "Keep it to 200 characters."})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callBackgroundDeadline)
	defer cancel()
	llm, cb, _, model := docAIRun.Resolve(ctx, userInfo.UserDgraphInfo.Uuid)
	if llm == nil || cb == nil {
		// AI was switched off between the gate and here: doc AI's answer.
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": aiSwitchedOffMsg})
		return
	}
	if err := cb.Allow(); err != nil {
		breakerRefusal(w, err)
		return
	}
	recipe, err := business.MakeCallRecipe(ctx, llm, cb, words)
	switch {
	case err == nil:
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": map[string]any{"recipe": recipe, "model": model}})
	case errors.Is(err, ai.ErrUserTokenBudgetExceeded), errors.Is(err, ai.ErrWorkspaceTokenBudgetExceeded):
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
	default:
		// Only our own words reach the log: a provider's error can quote what it
		// was sent, and the unreadable error says how long the answer was.
		why := ai.FriendlyProviderError(err)
		if errors.Is(err, business.ErrUnreadableRecipe) {
			why = err.Error()
		}
		if !errors.Is(err, context.Canceled) {
			helpers.LogWarnWithContext(ctx, "controllers/CallBackground: no recipe: %s", why)
		}
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": callBackgroundFailed})
	}
}
