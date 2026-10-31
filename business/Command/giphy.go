package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	commandAdapter "github.com/akashc777/OneCamp/adapter/Command"
	"github.com/akashc777/OneCamp/helpers/safefetch"
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

const (
	giphyAppSlug = "giphy"
	// giphyRating is the content rating every Giphy request asks for.
	giphyRating = "pg-13"
	// giphyCommandResults is how many GIFs /giphy loads to shuffle through.
	giphyCommandResults = 15
	// giphyPickerPage is one page of the composer picker's grid.
	giphyPickerPage = 24
	// giphyMaxTerm and giphyMaxOffset are Giphy's own limits on a search's
	// length and on how deep its pages go.
	giphyMaxTerm   = 50
	giphyMaxOffset = 4999
)

// giphyAPI is Giphy's GIF API; a unit test points it at a stand-in.
var giphyAPI = "https://api.giphy.com/v1/gifs"

var (
	// ErrGiphyBadRequest is a picker request Giphy can't serve: a search past
	// giphyMaxTerm characters, or an offset outside 0..giphyMaxOffset.
	ErrGiphyBadRequest = errors.New("giphy: bad request")
	// ErrGiphyUnavailable is Giphy failing to answer, or refusing the key.
	ErrGiphyUnavailable = errors.New("giphy: unavailable")
)

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

	gifs, _, err := giphyQuery(ctx, apiKey, term, 0, giphyCommandResults)
	if err != nil || len(gifs) == 0 {
		return ephemeral(fmt.Sprintf("No GIFs found for “%s”.", term)), nil
	}
	urls := make([]string, len(gifs))
	for i, g := range gifs {
		urls[i] = g.Send.URL
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

func handleGiphyInteract(ctx context.Context, cc CommandContext, ir Press) (*commandAdapter.CommandResponse, error) {
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
// bag. Returns "" if the app isn't installed, is turned off, or has no key.
func giphyAPIKey(ctx context.Context) (string, error) {
	app, err := slashModel.GetAppBySlug(ctx, giphyAppSlug)
	if err != nil || app == nil || !app.IsEnabled {
		return "", err
	}
	return GetAppSecret(ctx, app.Id, "api_key")
}

// giphyImage is one rendition as Giphy answers it, its sizes as strings.
type giphyImage struct {
	URL    string `json:"url"`
	WebP   string `json:"webp"`
	Width  string `json:"width"`
	Height string `json:"height"`
}

// rendition is the image's GIF, or its webp when webp is set and Giphy has one.
func (i giphyImage) rendition(webp bool) commandAdapter.GiphyRendition {
	u := i.URL
	if webp && i.WebP != "" {
		u = i.WebP
	}
	w, _ := strconv.Atoi(i.Width)
	h, _ := strconv.Atoi(i.Height)
	return commandAdapter.GiphyRendition{URL: u, Width: w, Height: h}
}

// giphyQuery asks Giphy for one page of GIFs from offset, trending when term
// is empty and a search for it otherwise, and returns them with Giphy's total
// for the query. /giphy, the picker and the key's Test all ask through it.
func giphyQuery(ctx context.Context, apiKey, term string, offset, limit int) ([]commandAdapter.GiphyGIF, *int, error) {
	params := url.Values{
		"api_key": {apiKey},
		"limit":   {strconv.Itoa(limit)},
		"offset":  {strconv.Itoa(offset)},
		"rating":  {giphyRating},
	}
	endpoint := giphyAPI + "/trending?"
	if term != "" {
		params.Set("q", term)
		endpoint = giphyAPI + "/search?"
	}
	endpoint += params.Encode()
	// Errors leave without the request's address: its query holds the
	// workspace's key, and callers log them and show them to admins.
	if err := forwardCheck(endpoint); err != nil {
		return nil, nil, safefetch.UnwrapURLError(err)
	}

	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, safefetch.UnwrapURLError(err)
	}
	resp, err := forwardClient().Do(req)
	if err != nil {
		return nil, nil, safefetch.UnwrapURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("giphy returned %d", resp.StatusCode)
	}

	var parsed struct {
		Data []struct {
			ID     string `json:"id"`
			Title  string `json:"title"`
			Images struct {
				FixedWidth      giphyImage `json:"fixed_width"`
				FixedHeight     giphyImage `json:"fixed_height"`
				DownsizedLarge  giphyImage `json:"downsized_large"`
				DownsizedMedium giphyImage `json:"downsized_medium"`
				Original        giphyImage `json:"original"`
			} `json:"images"`
		} `json:"data"`
		Pagination struct {
			TotalCount int `json:"total_count"`
		} `json:"pagination"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, nil, err
	}

	gifs := make([]commandAdapter.GiphyGIF, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		// A message sends a display-sized rendition: fixed_height (200 px
		// tall), else downsized_large, downsized_medium, the original.
		// downsized_medium alone is tiny (~200 px), which made sent GIFs look
		// small.
		var send commandAdapter.GiphyRendition
		for _, img := range []giphyImage{d.Images.FixedHeight, d.Images.DownsizedLarge, d.Images.DownsizedMedium, d.Images.Original} {
			if img.URL != "" {
				send = img.rendition(false)
				break
			}
		}
		if send.URL == "" {
			continue
		}
		// The grid draws the 200 px wide rendition, as webp where Giphy has
		// one, a fraction of the GIF's bytes.
		preview := send
		if d.Images.FixedWidth.URL != "" {
			preview = d.Images.FixedWidth.rendition(true)
		}
		gifs = append(gifs, commandAdapter.GiphyGIF{ID: d.ID, Title: d.Title, Preview: preview, Send: send})
	}
	// The next page starts after every GIF Giphy sent, the skipped ones too:
	// counting only those kept repeated a page's last GIF on the next.
	return gifs, nextGiphyOffset(offset, len(parsed.Data), parsed.Pagination.TotalCount), nil
}

// nextGiphyOffset is where the page after count GIFs from offset starts, or
// nil when that was the last page Giphy will serve.
func nextGiphyOffset(offset, count, total int) *int {
	next := offset + count
	if count == 0 || next >= total || next > giphyMaxOffset {
		return nil
	}
	return &next
}

// GiphyPicker answers the composer's GIF picker: a page of trending GIFs, or
// of a search for term, from offset. A page is kept for everyone for a few
// minutes (registry.GiphyPage), so people browsing the same GIFs spend the
// workspace's Giphy quota once. With no key, or the app turned off, it
// answers Configured: false and asks Giphy nothing.
func GiphyPicker(ctx context.Context, term string, offset int) (*commandAdapter.GiphyPage, error) {
	if utf8.RuneCountInString(term) > giphyMaxTerm || offset < 0 || offset > giphyMaxOffset {
		return nil, ErrGiphyBadRequest
	}
	apiKey, err := giphyAPIKey(ctx)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		return &commandAdapter.GiphyPage{Items: []commandAdapter.GiphyGIF{}}, nil
	}

	args := []string{strings.ToLower(term), strconv.Itoa(offset)}
	var page commandAdapter.GiphyPage
	if found, _ := redisStore.GetJSON(ctx, registry.GiphyPage, args, &page); found {
		return &page, nil
	}
	gifs, next, err := giphyQuery(ctx, apiKey, term, offset, giphyPickerPage)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGiphyUnavailable, err)
	}
	page = commandAdapter.GiphyPage{Configured: true, Items: gifs, NextOffset: next}
	_ = redisStore.SetJSON(ctx, registry.GiphyPage, args, &page)
	return &page, nil
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
	gifs, _, err := giphyQuery(ctx, key, "hello", 0, giphyCommandResults)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "401") || strings.Contains(msg, "403") {
			return &commandAdapter.AppTestResult{Success: false, Message: "Giphy rejected this API key (401/403). Double-check you copied the full key with no extra spaces, then save and test again."}
		}
		return &commandAdapter.AppTestResult{Success: false, Message: fmt.Sprintf("Couldn't reach Giphy: %v", err)}
	}
	return &commandAdapter.AppTestResult{Success: true, Message: fmt.Sprintf("Success — Giphy returned %d results. Your key works.", len(gifs))}
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
