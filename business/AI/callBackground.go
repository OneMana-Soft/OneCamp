package business

// Call backgrounds made from words (meet-09): a person describes a background
// for their camera and their workspace's own model answers with a RECIPE:
// a few colours, one of the built-in compositions and up to three extras.
// The browser builds the picture from that composition in those colours with
// the built-in backgrounds' painter (lib/calls/processors/backdrops.ts,
// sceneFromRecipe), so the compositions exist in one copy only, and whatever
// the model answers can only ever become those shapes in those colours.
//
// Why a recipe and not a whole scene (h4b ruling 12): qwen3 4B on 12 CPUs
// reads ~35-60 tokens a second and writes ~2.5-10, so a scene of every shape
// (~350 tokens out, ~600 in) ran past the 60 s deadline nine times in ten. A
// recipe is ~60 tokens out from a ~300-token prompt.
//
// The words go to the model and nowhere else: nothing here logs or keeps
// them, and an unreadable answer is reported by its length alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// CallRecipe is a background as the model chooses it. Palette is 3 to 5
// colours in this order: sky top, sky bottom, land, deep land, light (the
// browser fills the ones left out from the layout's own). Layout is one of
// callLayouts; Extras are from callExtras, each once, at most three.
type CallRecipe struct {
	Palette []string `json:"palette"`
	Layout  string   `json:"layout"`
	Extras  []string `json:"extras"`
}

// callLayouts are the built-in backgrounds' ids (BackdropId on the web).
var callLayouts = map[string]bool{"moss": true, "sun": true, "lake": true, "sky": true, "dusk": true, "berry": true}

// callExtras are what the painter's kinds can add: a glow, dots, a band.
var callExtras = map[string]bool{"sun": true, "moon": true, "stars": true, "glow": true, "mist": true}

// ErrUnreadableRecipe is a model's answer that, after its one corrective
// retry, still isn't a recipe.
var ErrUnreadableRecipe = errors.New("the model's recipe was unreadable")

// MaxCallSceneWords is how long a description may be, in characters.
const MaxCallSceneWords = 200

var sceneColour = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

const callRecipePrompt = `You choose a video call background. Answer with ONE JSON object and nothing else:
{"palette": [3 to 5 "#RRGGBB" colours in this order: sky top, sky bottom, land, deep land, light], "layout": one id, "extras": [0 to 3 names]}
Layouts: "moss" rolling hills, light high right; "sun" big low sun over flat land; "lake" calm water, light high left; "sky" open sky, soft clouds; "dusk" low light, fading sky; "berry" soft round lights, no land.
Extras: "sun", "moon", "stars", "glow", "mist".
Colours that fit the words; keep it calm.
Example, for "night sky over quiet hills":
{"palette":["#0B1D3A","#1E3A6B","#2A4B7C","#14284F","#F4E3B1"],"layout":"moss","extras":["stars","moon"]}`

// callRecipeOptions: a person is waiting, a little variety is welcome, and a
// recipe is ~60 tokens.
var callRecipeOptions = ai.ChatOptions{LowLatency: true, Temperature: 0.7, MaxTokens: 120}

// MakeCallRecipe asks the person's model (llm, its breaker cb) for a recipe
// of their words: JSON mode, one corrective retry, then ErrUnreadableRecipe,
// which says only how long the answer was.
func MakeCallRecipe(ctx context.Context, llm ai.LLMProvider, cb *ai.CircuitBreaker, words string) (CallRecipe, error) {
	valid := func(s string) bool { _, ok := readCallRecipe(s); return ok }
	out, err := ai.ChatJSONWithRetry(ctx, llm, cb, callRecipePrompt, "The background the person wants:\n"+words, callRecipeOptions, valid)
	if err != nil {
		return CallRecipe{}, err
	}
	recipe, ok := readCallRecipe(out)
	if !ok {
		return CallRecipe{}, fmt.Errorf("%w (%d bytes)", ErrUnreadableRecipe, len(out))
	}
	return recipe, nil
}

// readCallRecipe is a model's answer as a recipe, or false when it isn't
// one: it needs three #RRGGBB colours and a layout from the list. Colours that
// aren't #RRGGBB, extras not in the set, repeats and fields it doesn't know are
// dropped; the first five colours and three extras are kept, upper-cased hex.
func readCallRecipe(raw string) (CallRecipe, bool) {
	js := extractJSONObject(StripReasoning(raw))
	if js == "" {
		return CallRecipe{}, false
	}
	var loose struct {
		Palette []json.RawMessage `json:"palette"`
		Layout  string            `json:"layout"`
		Extras  []json.RawMessage `json:"extras"`
	}
	if json.Unmarshal([]byte(js), &loose) != nil || !callLayouts[loose.Layout] {
		return CallRecipe{}, false
	}
	recipe := CallRecipe{Layout: loose.Layout, Extras: []string{}}
	for _, c := range loose.Palette {
		var s string
		if json.Unmarshal(c, &s) == nil && sceneColour.MatchString(s) && len(recipe.Palette) < 5 {
			recipe.Palette = append(recipe.Palette, strings.ToUpper(s))
		}
	}
	seen := map[string]bool{}
	for _, e := range loose.Extras {
		var s string
		if json.Unmarshal(e, &s) == nil && callExtras[s] && !seen[s] && len(recipe.Extras) < 3 {
			seen[s] = true
			recipe.Extras = append(recipe.Extras, s)
		}
	}
	if len(recipe.Palette) < 3 {
		return CallRecipe{}, false
	}
	return recipe, true
}
