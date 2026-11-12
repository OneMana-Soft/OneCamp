package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	ai "github.com/akashc777/OneCamp/services/AI"
)

// recipeLLM answers each chat with the next of its answers and counts the chats.
type recipeLLM struct {
	ai.LLMProvider
	answers []string
	asked   [][]ai.ChatMessage
	opts    []ai.ChatOptions
}

func (m *recipeLLM) Chat(_ context.Context, msgs []ai.ChatMessage, opts ai.ChatOptions) (string, error) {
	m.asked = append(m.asked, msgs)
	m.opts = append(m.opts, opts)
	if len(m.asked) > len(m.answers) {
		return "", errors.New("asked more often than the test expected")
	}
	return m.answers[len(m.asked)-1], nil
}

// goodRecipe is a recipe as the model should write it.
const goodRecipe = `{"palette":["#0B1D3A","#1E3A6B","#2A4B7C","#14284F","#F4E3B1"],"layout":"moss","extras":["stars","moon"]}`

func TestAGoodRecipeIsKeptAsSent(t *testing.T) {
	recipe, ok := readCallRecipe("Here it is:\n```json\n" + goodRecipe + "\n```")
	if !ok {
		t.Fatal("a good recipe was refused")
	}
	if got, _ := json.Marshal(recipe); string(got) != goodRecipe {
		t.Errorf("the recipe changed on the way through:\n got %s\nwant %s", got, goodRecipe)
	}
}

func TestEveryBadFieldOfARecipeIsDroppedOrRefused(t *testing.T) {
	recipe, ok := readCallRecipe(`{"onload":"alert(1)","palette":["#0b1d3a","red","#FFF","url(x)","#334455","#AABBCC","#112233","#445566","#778899"],` +
		`"layout":"lake","extras":["stars","image","stars","script","moon","mist","sun"],"src":"https://example.com/x.png"}`)
	if !ok {
		t.Fatal("a recipe with good parts was refused whole")
	}
	if want := []string{"#0B1D3A", "#334455", "#AABBCC", "#112233", "#445566"}; fmt.Sprint(recipe.Palette) != fmt.Sprint(want) {
		t.Errorf("palette %v, want %v: only #RRGGBB colours, upper-cased, the first five", recipe.Palette, want)
	}
	if want := []string{"stars", "moon", "mist"}; fmt.Sprint(recipe.Extras) != fmt.Sprint(want) {
		t.Errorf("extras %v, want %v: known names only, once each, at most three", recipe.Extras, want)
	}
	out, _ := json.Marshal(recipe)
	for _, word := range []string{"onload", "alert", "src", "example.com", "url(", "red", "image", "script"} {
		if strings.Contains(string(out), word) {
			t.Errorf("%q survived into the recipe: %s", word, out)
		}
	}
	for name, raw := range map[string]string{
		"prose":          "A misty forest at dawn, with pines.",
		"two colours":    `{"palette":["#000000","#FFFFFF","blue"],"layout":"moss","extras":[]}`,
		"unknown layout": `{"palette":["#000000","#FFFFFF","#FF0000"],"layout":"forest","extras":[]}`,
		"no layout":      `{"palette":["#000000","#FFFFFF","#FF0000"],"extras":["sun"]}`,
	} {
		if _, ok := readCallRecipe(raw); ok {
			t.Errorf("%s: read as a recipe", name)
		}
	}
	if r, ok := readCallRecipe(`{"palette":["#000000","#FFFFFF","#FF0000"],"layout":"berry"}`); !ok || r.Extras == nil {
		t.Errorf("no extras is a good recipe, written as an empty list: %+v %v", r, ok)
	}
}

// A 4B model on a CPU reads ~60 tokens a second and writes ~10 (h4b ruling
// 12): the prompt and the answer must both be short to land inside 60 s.
func TestTheRecipeAskFitsASmallModel(t *testing.T) {
	if n := ai.EstimateTokens(callRecipePrompt); n > 350 {
		t.Errorf("the recipe prompt is ~%d tokens, want at most 350", n)
	}
	if callRecipeOptions.MaxTokens == 0 || callRecipeOptions.MaxTokens > 120 {
		t.Errorf("MaxTokens %d, want 1..120", callRecipeOptions.MaxTokens)
	}
	for id := range callLayouts {
		if !strings.Contains(callRecipePrompt, `"`+id+`"`) {
			t.Errorf("layout %q isn't offered in the prompt", id)
		}
	}
	for id := range callExtras {
		if !strings.Contains(callRecipePrompt, `"`+id+`"`) {
			t.Errorf("extra %q isn't offered in the prompt", id)
		}
	}
}

func TestAnUnreadableRecipeIsAskedOnceMoreThenRefusedInWords(t *testing.T) {
	ctx := context.Background()
	cb := ai.NewCircuitBreaker(5, time.Minute)

	mended := &recipeLLM{answers: []string{"Sure! Picture a misty pine forest at dawn.", goodRecipe}}
	recipe, err := MakeCallRecipe(ctx, mended, cb, "Misty pine forest at dawn")
	if err != nil || recipe.Layout != "moss" {
		t.Fatalf("prose, then a good recipe: got %+v, %v", recipe, err)
	}
	if len(mended.asked) != 2 {
		t.Errorf("the model was asked %d times, want 2 (one corrective retry)", len(mended.asked))
	}
	if first := mended.asked[0]; len(first) != 2 || first[0].Role != "system" || !strings.Contains(first[1].Content, "Misty pine forest at dawn") {
		t.Errorf("the words should go to the model as the person's message, under the recipe instructions: %+v", first)
	}
	if o := mended.opts[0]; !o.JSONMode || !o.LowLatency {
		t.Errorf("a recipe is asked in JSON mode, low latency: %+v", o)
	}

	prose := &recipeLLM{answers: []string{"A forest.", "Still a forest, in words."}}
	_, err = MakeCallRecipe(ctx, prose, cb, "Misty pine forest at dawn")
	if !errors.Is(err, ErrUnreadableRecipe) {
		t.Fatalf("prose twice should be the unreadable-recipe error, got %v", err)
	}
	if len(prose.asked) != 2 {
		t.Errorf("the model was asked %d times, want exactly 2", len(prose.asked))
	}
	if strings.Contains(err.Error(), "forest") {
		t.Errorf("the error carries the model's words: %q; it may say only how long the answer was", err)
	}
}

// Ruling 12, "hex only": a colour is # and six hex digits, nothing before or
// after, so nothing a model tacks on can ride into the browser as a colour.
func TestAColourIsSixHexDigitsAndNothingMore(t *testing.T) {
	recipe, ok := readCallRecipe(`{"palette":["#0B1D3A;url(x)","#1E3A6B","x#2A4B7C","#2A4B7C0","#2A4B7C","#F4E3B1"],"layout":"sky"}`)
	if !ok {
		t.Fatal("three clean colours were refused")
	}
	if want := []string{"#1E3A6B", "#2A4B7C", "#F4E3B1"}; fmt.Sprint(recipe.Palette) != fmt.Sprint(want) {
		t.Errorf("palette %v, want %v: a colour with anything around its six digits is dropped", recipe.Palette, want)
	}
	if _, ok := readCallRecipe(`{"palette":["#0B1D3A;url(x)","#1E3A6B","#F4E3B1"],"layout":"sky"}`); ok {
		t.Error("two clean colours and one with a tail were read as a recipe of three")
	}
}

// A reasoning model thinks out loud first; what it thought, braces and all, is
// not the recipe.
func TestAReasoningModelsThinkingIsNotTheRecipe(t *testing.T) {
	recipe, ok := readCallRecipe("<think>Night, calm. Something like {\"layout\":\"sun\"}? No: hills.</think>\n" + goodRecipe)
	if !ok {
		t.Fatal("a recipe after the model's thinking was refused")
	}
	if got, _ := json.Marshal(recipe); string(got) != goodRecipe {
		t.Errorf("read the thinking, not the recipe:\n got %s\nwant %s", got, goodRecipe)
	}
}

// A small model in JSON mode sometimes wraps its one answer in a list. The
// shared extractor takes the object out of it, as it does out of prose or a
// fence, and the server answers the object alone, which is all the browser's
// sceneFromRecipe reads; refusing would only spend the retry. Two objects are
// two answers, and neither is taken.
func TestARecipeInAListOfOneIsReadAndAListOfTwoIsNot(t *testing.T) {
	recipe, ok := readCallRecipe("[" + goodRecipe + "]")
	if got, _ := json.Marshal(recipe); !ok || string(got) != goodRecipe {
		t.Errorf("a recipe in a list of one: %s %v, want %s", got, ok, goodRecipe)
	}
	if _, ok := readCallRecipe("[" + goodRecipe + "," + goodRecipe + "]"); ok {
		t.Error("a list of two recipes was read as one")
	}
}
