package business

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers"
	slashModel "github.com/akashc777/OneCamp/models/postgres/SlashCommand"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/google/uuid"
)

// giphy.go — a first-class, built-in interactive command that doubles as the
// reference implementation for an interactive app. /giphy <term> shows a GIF
// with Shuffle / Send / Cancel, exactly like Slack's Giphy app.
//
// The Giphy API key is NOT an env var: an admin installs a "giphy" app
// (kind=external) and stores the key in its encrypted secret bag under
// "api_key". This keeps the platform generic — Giphy is just an installed app
// whose command happens to be handled in-process for the best UX.

const giphyAppSlug = "giphy"

func init() {
	Register("giphy", handleGiphy)
	RegisterInteraction("giphy", handleGiphyInteract)
	RegisterAppTest(giphyAppSlug, testGiphy)
}

type giphyState struct {
	Query string   `json:"query"`
	URLs  []string `json:"urls"`
	Index int      `json:"index"`
}

func handleGiphy(ctx context.Context, cc CommandContext) (*commandAdapter.CommandResponse, error) {
	term := strings.TrimSpace(cc.Text)
	if term == "" {
		return errorResponse("Usage: `/giphy <search term>`"), nil
	}

	apiKey, err := giphyAPIKey(ctx)
	if err != nil || apiKey == "" {
		return errorResponse("Giphy isn't configured. An admin can add the Giphy API key under Apps."), nil
	}

	urls, err := giphySearch(ctx, apiKey, term)
	if err != nil || len(urls) == 0 {
		return ephemeral(fmt.Sprintf("No GIFs found for “%s”.", term)), nil
	}

	state := &giphyState{Query: term, URLs: urls, Index: 0}
	saveGiphyState(ctx, cc.TriggerID, state)

	return &commandAdapter.CommandResponse{
		ResponseType: "ephemeral",
		Ephemeral:    true,
		Blocks:       renderGiphyBlocks(state),
		PreloadURLs:  urls, // warm the whole result set so Shuffle is instant
		TriggerID:    cc.TriggerID,
	}, nil
}

func handleGiphyInteract(ctx context.Context, cc CommandContext, ir commandAdapter.InteractRequest) (*commandAdapter.CommandResponse, error) {
	state, ok := loadGiphyState(ctx, ir.TriggerID)
	if !ok {
		return errorResponse("This GIF picker has expired. Try `/giphy` again."), nil
	}

	switch ir.ActionID {
	case "giphy_shuffle":
		// Advance the index atomically so rapid double-clicks (or two devices)
		// can't race on the shared state.
		_ = redisStore.UpdateJSONAtomic(
			ctx, registry.CommandInteraction, []string{ir.TriggerID}, registry.CommandInteraction.TTL,
			func(cur giphyState, found bool) giphyState {
				if found && len(cur.URLs) > 0 {
					cur.Index = (cur.Index + 1) % len(cur.URLs)
				}
				return cur
			},
		)
		if s, ok := loadGiphyState(ctx, ir.TriggerID); ok {
			state = s
		}
		return &commandAdapter.CommandResponse{
			ResponseType:    "ephemeral",
			Ephemeral:       true,
			Blocks:          renderGiphyBlocks(state),
			ReplaceOriginal: true,
			TriggerID:       ir.TriggerID,
		}, nil

	case "giphy_send":
		if state.Index < 0 || state.Index >= len(state.URLs) {
			return errorResponse("Nothing to send."), nil
		}
		gifURL := state.URLs[state.Index]
		clearGiphyState(ctx, ir.TriggerID)
		// Post the GIF as a normal message via a client action so it goes
		// through the surface's own send path (channel/DM/group), attributed
		// to the user — matching Slack's behavior.
		return &commandAdapter.CommandResponse{
			ResponseType:    "ephemeral",
			Ephemeral:       true,
			ReplaceOriginal: true,
			TriggerID:       ir.TriggerID,
			ClientAction: &commandAdapter.ClientAction{
				Type:    "post_message",
				Payload: map[string]string{"html": fmt.Sprintf(`<p><img src="%s" alt="%s"/></p>`, gifURL, htmlEscape(state.Query))},
			},
		}, nil

	case "giphy_cancel":
		clearGiphyState(ctx, ir.TriggerID)
		return &commandAdapter.CommandResponse{
			ResponseType:    "ephemeral",
			Ephemeral:       true,
			ReplaceOriginal: true,
			TriggerID:       ir.TriggerID,
			Text:            "Cancelled.",
		}, nil
	}
	return errorResponse("Unknown action."), nil
}

func renderGiphyBlocks(s *giphyState) []commandAdapter.Block {
	current := ""
	if s.Index >= 0 && s.Index < len(s.URLs) {
		current = s.URLs[s.Index]
	}
	return []commandAdapter.Block{
		{
			Type: "context",
			Text: &commandAdapter.BlockText{Type: "mrkdwn", Text: fmt.Sprintf("Giphy results for *%s* (%d/%d)", s.Query, s.Index+1, len(s.URLs))},
		},
		{
			Type:     "image",
			ImageURL: current,
			AltText:  s.Query,
		},
		{
			Type: "actions",
			Elements: []commandAdapter.BlockElement{
				{Type: "button", Text: &commandAdapter.BlockText{Type: "plain_text", Text: "Shuffle"}, ActionID: "giphy_shuffle"},
				{Type: "button", Text: &commandAdapter.BlockText{Type: "plain_text", Text: "Send"}, ActionID: "giphy_send", Style: "primary"},
				{Type: "button", Text: &commandAdapter.BlockText{Type: "plain_text", Text: "Cancel"}, ActionID: "giphy_cancel", Style: "danger"},
			},
		},
	}
}

// giphyAPIKey reads the key from the installed "giphy" app's encrypted secret
// bag. Returns "" if the app isn't installed or has no key.
func giphyAPIKey(ctx context.Context) (string, error) {
	app, err := slashModel.GetAppBySlug(ctx, giphyAppSlug)
	if err != nil || app == nil {
		return "", err
	}
	return GetAppSecret(ctx, app.Id, "api_key")
}

// giphySearch queries the Giphy search API and returns downsized GIF URLs.
func giphySearch(ctx context.Context, apiKey, term string) ([]string, error) {
	endpoint := "https://api.giphy.com/v1/gifs/search?" + url.Values{
		"api_key": {apiKey},
		"q":       {term},
		"limit":   {"15"},
		"rating":  {"pg-13"},
	}.Encode()

	if _, err := helpers.ValidateOutboundURL(endpoint, false); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	client := helpers.SSRFSafeClient(false)
	client.Timeout = 8 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("giphy returned %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		Data []struct {
			Images struct {
				DownsizedLarge struct {
					URL string `json:"url"`
				} `json:"downsized_large"`
				FixedHeight struct {
					URL string `json:"url"`
				} `json:"fixed_height"`
				DownsizedMedium struct {
					URL string `json:"url"`
				} `json:"downsized_medium"`
				Original struct {
					URL string `json:"url"`
				} `json:"original"`
			} `json:"images"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	var urls []string
	for _, d := range parsed.Data {
		// Prefer a display-sized rendition: fixed_height (480p tall, sharp and
		// reasonably small) → downsized_large → downsized_medium → original.
		// downsized_medium alone is tiny (~200px), which made sent GIFs look
		// small.
		u := helpers.FirstNonEmpty(
			d.Images.FixedHeight.URL,
			d.Images.DownsizedLarge.URL,
			d.Images.DownsizedMedium.URL,
			d.Images.Original.URL,
		)
		if u != "" {
			urls = append(urls, u)
		}
	}
	return urls, nil
}

// testGiphy hits the Giphy search API with the stored key (registered as the
// app's Test probe via RegisterAppTest). A 200 with the expected JSON shape
// proves the key is valid; 401/403 means a bad key.
func testGiphy(ctx context.Context, appID uuid.UUID) *commandAdapter.AppTestResult {
	key, err := GetAppSecret(ctx, appID, "api_key")
	if err != nil {
		return &commandAdapter.AppTestResult{Success: false, Message: "Couldn't read the stored API key."}
	}
	if key == "" {
		return &commandAdapter.AppTestResult{Success: false, Message: "No API key set. Paste your Giphy API key and save first."}
	}
	urls, err := giphySearch(ctx, key, "hello")
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "401") || strings.Contains(msg, "403") {
			return &commandAdapter.AppTestResult{Success: false, Message: "Giphy rejected this API key (401/403). Double-check you copied the full key with no extra spaces, then save and test again."}
		}
		return &commandAdapter.AppTestResult{Success: false, Message: fmt.Sprintf("Couldn't reach Giphy: %v", err)}
	}
	return &commandAdapter.AppTestResult{Success: true, Message: fmt.Sprintf("Success — Giphy returned %d results. Your key works.", len(urls))}
}

func saveGiphyState(ctx context.Context, triggerID string, s *giphyState) {
	// Store the struct directly (single JSON encoding). This MUST match how
	// UpdateJSONAtomic reads/writes the same key — it unmarshals straight into
	// giphyState and writes json.Marshal(struct). A manual pre-marshal here
	// would double-encode the value, so the atomic shuffle path would fail to
	// decode it, silently reset the state, and the next load would report the
	// picker as "expired".
	_ = redisStore.SetJSON(ctx, registry.CommandInteraction, []string{triggerID}, s)
}

func loadGiphyState(ctx context.Context, triggerID string) (*giphyState, bool) {
	var s giphyState
	found, err := redisStore.GetJSON(ctx, registry.CommandInteraction, []string{triggerID}, &s)
	if !found || err != nil {
		return nil, false
	}
	return &s, true
}

func clearGiphyState(ctx context.Context, triggerID string) {
	_ = redisStore.Delete(ctx, registry.CommandInteraction, []string{triggerID})
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}
