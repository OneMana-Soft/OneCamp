// Package miro implements the Import Provider interface for Miro boards
// (miro-37, W-2): a team brings its boards over in an afternoon, drawn as
// OneCamp boards.
//
// Source: Miro's REST API v2 only, from its public reference (the OpenAPI
// file in github.com/miroapp/api-clients). Auth: a token the admin pastes
// (owner decision O3): they make an app in a free Developer team in Miro,
// give it boards:read, install it on their team and copy its token. No
// OneCamp Miro app, and nobody signs in to Miro for us.
//
// Mapping (convert.go): sticky notes → notes in the colour that keeps
// Miro's, with their tags; shapes → the nearest shape, with their words;
// text → text; frames → frames holding their items; connectors → arrows
// bound to what they join, routed and capped as Miro's; pictures → the
// board's own pictures (kept through the board's upload, scanned); cards and
// app cards → cards; files, embeds and link previews → cards that name and
// open them; what the API can't read → a labelled placeholder. Comments,
// votes, timers, Talktracks and freehand drawings aren't in the API: the
// plan says so.
//
// Rate limits: Miro charges each user of an app credits a minute; listing
// items is level 2, about 1,000 calls a minute. The import keeps to ten a
// second and waits out a 429 until the time its X-RateLimit-Reset names.
package miro

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const providerName = importModels.ProviderMiro

// defaultAPIBase is Miro's API.
const defaultAPIBase = "https://api.miro.com"

// optBoardID is the board the admin picked; none picked imports every board
// the token can see.
const optBoardID = "board_id"

const (
	pageSize = 50 // Miro's most a page holds
	// maxBoards is the most boards one import reads.
	maxBoards = 500
	// maxItems is the most items one board brings: about 14 MB of drawing.
	maxItems = 20_000
	// bigBoard is where the plan says a board opens slowly here, until the
	// board's speed work (W-4) is measured past it.
	bigBoard = 4_000
	// styleReads is the most items read one at a time for a style the list
	// left out.
	styleReads = 2_000
	// maxTags is the most of a board's tags read for its notes.
	maxTags = 200
	// maxBody is the largest answer read.
	maxBody = 32 << 20
)

// Provider implements importProvider.Provider for Miro.
type Provider struct {
	// A board brings no tasks.
	importProvider.NoTasks

	rl     importProvider.Limiter
	base   string
	client *http.Client
	// token is the job's token; download fetches a picture from the address
	// Miro hands out for it. Fields, so tests can stand them in.
	token    func(ctx context.Context, j *importModels.Job) (string, error)
	download func(ctx context.Context, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error)
	now      func() time.Time
}

func New() *Provider {
	p := &Provider{
		rl:       importProvider.NewSleepLimiter(100*time.Millisecond, 5),
		base:     defaultAPIBase,
		client:   importProvider.SharedAPIClient,
		download: importProvider.DefaultFetchAttachment,
		now:      time.Now,
	}
	p.token = p.loadToken
	return p
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapBoards
}

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if t.AccessToken == "" {
		return "", errors.New("empty miro token")
	}
	return t.AccessToken, nil
}

// ─── HTTP ──────────────────────────────────────────────────────────

// errNotFound is a board or item Miro says isn't there.
var errNotFound = errors.New("miro: not found")

// statusRules reads what status leaves of Miro's answers. Only a 401 refuses
// the token: a 403 is about the token's scope or one board.
var statusRules = importProvider.StatusRules{Name: "Miro", OnlyUnauthorized: true}

// get asks Miro for path with query, as the token, into out: a refused token
// matches ErrTokenRejected, a 429 waits until X-RateLimit-Reset.
func (p *Provider) get(ctx context.Context, tok, path string, query url.Values, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	address := p.base + path
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := p.status(resp); err != nil {
		return err
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
}

// status is what an answer's status means to the import.
func (p *Provider) status(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return errNotFound
	case http.StatusTooManyRequests:
		return &importProvider.ErrRateLimited{RetryAfter: p.retryAfter(resp.Header), Reason: "Miro 429"}
	case http.StatusForbidden:
		return fmt.Errorf("Miro refused to read that (HTTP 403): the token needs the boards:read scope, and its app must be installed on the team that holds the board")
	}
	return statusRules.Check(resp)
}

// retryAfter is how long until Miro's rate window resets (X-RateLimit-Reset,
// seconds since 1970), from one second to a minute.
func (p *Provider) retryAfter(h http.Header) time.Duration {
	d := 10 * time.Second
	if v, err := strconv.ParseInt(strings.TrimSpace(h.Get("X-RateLimit-Reset")), 10, 64); err == nil && v > 0 {
		d = time.Until(time.Unix(v, 0))
	}
	return min(max(d, time.Second), time.Minute)
}

// ─── Boards ────────────────────────────────────────────────────────

// boards lists the boards the token can see, up to limit, and whether there
// are more.
func (p *Provider) boards(ctx context.Context, tok string, limit int) ([]board, bool, error) {
	var out []board
	for offset := 0; ; {
		var page boardsPage
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}, "sort": {"last_modified"}}
		if err := p.get(ctx, tok, "/v2/boards", q, &page); err != nil {
			return nil, false, err
		}
		for _, b := range page.Data {
			if len(out) == limit {
				return out, true, nil
			}
			out = append(out, b)
		}
		offset += len(page.Data)
		if len(page.Data) == 0 || (page.Total > 0 && offset >= page.Total) {
			return out, false, nil
		}
	}
}

// Validate asks for one board: a token Miro refuses is said now.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, _ importProvider.JobOptions) error {
	tok, err := p.token(ctx, j)
	if err != nil {
		return err
	}
	var page boardsPage
	return p.get(ctx, tok, "/v2/boards", url.Values{"limit": {"1"}}, &page)
}

// Discover lists the boards the token can see, most recently changed first,
// for the admin to pick one (or import them all).
func (p *Provider) Discover(ctx context.Context, _ string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	list, _, err := p.boards(ctx, token.AccessToken, maxBoards)
	if err != nil {
		return nil, err
	}
	out := make([]importProvider.DiscoverItem, 0, len(list))
	for _, b := range list {
		meta := map[string]any{}
		if b.ModifiedAt != "" {
			meta["modified_at"] = b.ModifiedAt
		}
		if b.Owner != nil && b.Owner.Name != "" {
			meta["owner"] = b.Owner.Name
		}
		out = append(out, importProvider.DiscoverItem{
			ID: string(b.ID), Name: firstOf(b.Name, "Untitled board"), Description: b.Description,
			URL: b.ViewLink, Kind: "board", Meta: meta,
		})
	}
	return out, nil
}

// planNotes are what every Miro import's plan says.
var planNotes = []string{
	"Miro's API doesn't give comments, votes, timers, Talktracks or freehand drawings, so they don't come over.",
	"Boards come over private to you; share them with your team once they're here (Miro's API doesn't give its members' email addresses).",
}

// Plan reads the boards to bring (the one picked, or every board the token
// sees) and how much is on each, and queues one chunk a board.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	tok, err := p.token(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	var list []board
	plan := &importProvider.Plan{Warnings: append([]string{}, planNotes...)}
	if picked := importProvider.PickedID(opts, optBoardID); picked != "" {
		var b board
		if err := p.get(ctx, tok, "/v2/boards/"+url.PathEscape(picked), nil, &b); err != nil {
			if errors.Is(err, errNotFound) {
				return nil, nil, errors.New("the Miro board you picked isn't visible to this token any more. Pick another board, or reconnect with a token that can see it")
			}
			return nil, nil, err
		}
		list = []board{b}
	} else {
		var more bool
		if list, more, err = p.boards(ctx, tok, maxBoards); err != nil {
			return nil, nil, err
		}
		if more {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("The token sees more than %d boards; the %d changed most recently come over. Import the rest board by board.", maxBoards, maxBoards))
		}
		if len(list) == 0 {
			return nil, nil, errors.New("this Miro token can't see any boards. Install its app on the team that holds your boards, then connect again")
		}
	}
	var chunks []*importModels.Chunk
	for _, b := range list {
		var page itemsPage
		if err := p.get(ctx, tok, "/v2/boards/"+url.PathEscape(string(b.ID))+"/items", url.Values{"limit": {"10"}}, &page); err != nil {
			return nil, nil, err
		}
		items := page.Total
		if items < len(page.Data) {
			items = len(page.Data)
		}
		name := firstOf(b.Name, "Untitled board")
		if items > maxItems {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%q has %d items, more than the %d one board brings, so it doesn't come over.", name, items, maxItems))
			continue
		}
		if items > bigBoard {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("%q has %d items; boards past %d open slowly here for now.", name, items, bigBoard))
		}
		plan.BoardCount++
		plan.ItemCount += items
		boardID := string(b.ID)
		chunks = append(chunks, &importModels.Chunk{
			Id: uuid.New(), ImportId: j.Id, ChunkType: importModels.ChunkBoards,
			ParentSourceId: &boardID, Status: importModels.ChunkStatusPending, MaxAttempts: 5,
		})
	}
	if plan.BoardCount == 0 {
		return nil, nil, errors.New("none of these Miro boards can come over: each has more than 20,000 items")
	}
	return plan, chunks, nil
}

// BoardOf reads one board, every item, connector, tag and group on it, and
// draws it.
func (p *Provider) BoardOf(ctx context.Context, j *importModels.Job, _ importProvider.JobOptions, boardID string, read func(items int)) (*importProvider.SourceBoard, error) {
	tok, err := p.token(ctx, j)
	if err != nil {
		return nil, err
	}
	path := "/v2/boards/" + url.PathEscape(boardID)
	var b board
	if err := p.get(ctx, tok, path, nil, &b); err != nil {
		return nil, err
	}
	src := source{tags: map[string][]string{}, groups: map[string][]string{}}
	var notes []importProvider.BoardNote
	for cursor := ""; ; {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page itemsPage
		if err := p.get(ctx, tok, path+"/items", q, &page); err != nil {
			return nil, err
		}
		src.items = append(src.items, page.Data...)
		if read != nil {
			read(len(src.items))
		}
		if len(src.items) >= maxItems {
			src.items = src.items[:maxItems]
			notes = append(notes, importProvider.BoardNote{Code: "BOARD_CUT", Count: maxItems,
				Msg: fmt.Sprintf("The board holds more than %d items; the first %d came over.", maxItems, maxItems)})
			break
		}
		if page.Cursor == "" || page.Cursor == cursor || len(page.Data) == 0 {
			break
		}
		cursor = page.Cursor
	}
	for cursor := ""; ; {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page connectorsPage
		if err := p.get(ctx, tok, path+"/connectors", q, &page); err != nil {
			return nil, err
		}
		src.connectors = append(src.connectors, page.Data...)
		if page.Cursor == "" || page.Cursor == cursor || len(page.Data) == 0 {
			break
		}
		cursor = page.Cursor
	}
	if err := p.readTags(ctx, tok, path, src.tags); err != nil {
		return nil, err
	}
	p.readGroups(ctx, tok, path, src.groups)
	if err := p.readStyles(ctx, tok, path, &src); err != nil {
		return nil, err
	}
	d := convert(src, p.now().UnixMilli())
	els := make([]map[string]any, len(d.elements))
	for i, el := range d.elements {
		els[i] = el
	}
	return &importProvider.SourceBoard{
		SourceID: boardID, Title: firstOf(b.Name, "Untitled board"), Description: b.Description,
		Elements: els, Pictures: d.pictures, Notes: append(notes, d.notes...), Items: len(src.items),
	}, nil
}

// readTags reads the board's tags and, for each, the items it is on.
func (p *Provider) readTags(ctx context.Context, tok, path string, into map[string][]string) error {
	var tags []tag
	for offset := 0; len(tags) < maxTags; {
		var page tagsPage
		q := url.Values{"limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
		if err := p.get(ctx, tok, path+"/tags", q, &page); err != nil {
			if errors.Is(err, errNotFound) {
				return nil
			}
			return err
		}
		tags = append(tags, page.Data...)
		offset += len(page.Data)
		if len(page.Data) == 0 || (page.Total > 0 && offset >= page.Total) {
			break
		}
	}
	for _, t := range tags {
		title := strings.TrimSpace(t.Title)
		if title == "" {
			continue
		}
		for offset := 0; ; {
			var page taggedPage
			q := url.Values{"tag_id": {string(t.ID)}, "limit": {strconv.Itoa(pageSize)}, "offset": {strconv.Itoa(offset)}}
			if err := p.get(ctx, tok, path+"/items", q, &page); err != nil {
				if errors.Is(err, errNotFound) {
					break
				}
				return err
			}
			for _, it := range page.Data {
				into[string(it.ID)] = append(into[string(it.ID)], title)
			}
			offset += len(page.Data)
			if len(page.Data) == 0 || (page.Total > 0 && offset >= page.Total) {
				break
			}
		}
	}
	return nil
}

// readGroups reads the board's groups, as best it can: a group the answer
// doesn't name its items for is left out, and so are all of them when Miro
// doesn't answer (the drawing is the same; only grouping is lost).
func (p *Provider) readGroups(ctx context.Context, tok, path string, into map[string][]string) {
	for cursor := ""; ; {
		q := url.Values{"limit": {strconv.Itoa(pageSize)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Data   []json.RawMessage `json:"data"`
			Cursor string            `json:"cursor"`
		}
		if err := p.get(ctx, tok, path+"/groups", q, &page); err != nil {
			return
		}
		for _, raw := range page.Data {
			if gid, items := groupItems(raw); gid != "" && len(items) > 1 {
				into[gid] = items
			}
		}
		if page.Cursor == "" || page.Cursor == cursor || len(page.Data) == 0 {
			return
		}
		cursor = page.Cursor
	}
}

// groupItems reads one group: its id and its items' ids, from "items" in its
// data or at its top, each a string or an object with an id.
func groupItems(raw json.RawMessage) (string, []string) {
	var g struct {
		ID    id              `json:"id"`
		Items json.RawMessage `json:"items"`
		Data  struct {
			Items json.RawMessage `json:"items"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &g) != nil {
		return "", nil
	}
	list := g.Data.Items
	if len(list) == 0 {
		list = g.Items
	}
	var ids []id
	if json.Unmarshal(list, &ids) != nil {
		var objs []struct {
			ID id `json:"id"`
		}
		if json.Unmarshal(list, &objs) != nil {
			return "", nil
		}
		for _, o := range objs {
			ids = append(ids, o.ID)
		}
	}
	out := make([]string, 0, len(ids))
	for _, i := range ids {
		if i != "" {
			out = append(out, string(i))
		}
	}
	return string(g.ID), out
}

// typedPath is the address of one item of a type, whose answer carries its
// style (the list's items may not).
var typedPath = map[string]string{
	"sticky_note": "sticky_notes", "shape": "shapes", "text": "texts", "frame": "frames", "card": "cards",
}

// readStyles reads, one at a time, the style of each item the list gave
// without one, up to styleReads; the rest come over in default colours and
// the report counts them.
func (p *Provider) readStyles(ctx context.Context, tok, path string, src *source) error {
	reads := 0
	for i := range src.items {
		it := &src.items[i]
		kind, styled := typedPath[it.Type], len(it.Style) > 0 && string(it.Style) != "null"
		if kind == "" || styled {
			continue
		}
		if reads == styleReads {
			src.styleless++
			continue
		}
		reads++
		var one item
		err := p.get(ctx, tok, path+"/"+kind+"/"+url.PathEscape(string(it.ID)), nil, &one)
		switch {
		case err == nil && len(one.Style) > 0:
			it.Style = one.Style
		case err == nil || errors.Is(err, errNotFound):
			src.styleless++
		default:
			return err
		}
	}
	return nil
}

// FetchAttachment reads a picture: Miro's address for an image (the item's
// imageUrl, on its API) answers, as the token, a download address good for
// about a minute, which is read at once and without the token.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, _ importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	tok, err := p.token(ctx, j)
	if err != nil {
		return "", 0, err
	}
	base, err := url.Parse(p.base)
	if err != nil {
		return "", 0, err
	}
	if !importProvider.CredentialAllowed(att.URL, base.Hostname()) {
		return "", 0, fmt.Errorf("a picture's address isn't on Miro's API: %w", importProvider.ErrAttachmentGone)
	}
	u, err := url.Parse(att.URL)
	if err != nil {
		return "", 0, err
	}
	q := u.Query()
	q.Set("format", "original")
	q.Set("redirect", "false")
	u.RawQuery = q.Encode()
	if err := p.rl.Wait(ctx); err != nil {
		return "", 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if err := p.status(resp); err != nil {
		if errors.Is(err, errNotFound) {
			return "", 0, importProvider.ErrAttachmentGone
		}
		return "", 0, err
	}
	kind, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if strings.HasPrefix(kind, "image/") {
		// The picture itself, when Miro sends it rather than where it is.
		n, err := io.Copy(dest, resp.Body)
		return kind, n, err
	}
	var where struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&where); err != nil || where.URL == "" {
		return "", 0, fmt.Errorf("Miro gave no address for the picture: %w", importProvider.ErrAttachmentGone)
	}
	return p.download(ctx, importProvider.SourceAttachment{SourceID: att.SourceID, Name: att.Name, URL: where.URL}, dest)
}
