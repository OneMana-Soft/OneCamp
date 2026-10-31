// Package airtable implements the Import Provider interface for Airtable
// (tables T-AT): a base becomes OneCamp tables, through the tables stage
// (CapTables, business/Import tables_worker.go) and the one importer of
// every source's tables (business/DataTable ImportBaseAs).
//
// Source: live API only ("api"), https://api.airtable.com/v0.
//
// Auth: a personal access token (airtable.com/create/tokens) with the
// scopes data.records:read and schema.bases:read, and access to the bases
// to bring, sent as a Bearer token. Stored encrypted via
// importModels.SaveToken like every other provider.
//
// What comes across, per base (options.bases narrows it; none is every base
// the token reaches):
//   - each table, its fields of the nearest OneCamp kind (fields.go), each
//     pair of linked fields as one relation shown from both tables;
//   - every record, its cells as the kind keeps them (people matched to
//     members by email), and its links;
//   - its views, as the nearest OneCamp view type.
//
// Airtable's API doesn't say how a rollup adds up, nor a view's filters,
// sorts and hidden fields, so a rollup keeps its values and a view comes
// across as its type alone; each is said in the import's notes. Files come
// across as files of their table, fetched and scanned as the rows are
// written.
//
// Rate limits: 5 requests a second per base. The limiter keeps every base
// of an import under that together; a 429 waits out Airtable's 30 seconds.
package airtable

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

const providerName = importModels.ProviderAirtable

// apiBase is Airtable's Web API; a variable so tests point it at a fake.
var apiBase = "https://api.airtable.com/v0"

// pageSize is how many records one list call reads (Airtable's most).
const pageSize = 100

// maxBases is the most bases one import lists.
const maxBases = 1000

// Provider implements importProvider.Provider and TableSource for Airtable.
type Provider struct {
	// A base brings tables only.
	importProvider.NoTasks

	rl importProvider.Limiter
}

// New constructs a provider with its own limiter: 5 a second, Airtable's
// limit for one base.
func New() *Provider {
	return &Provider{rl: importProvider.NewSleepLimiter(200*time.Millisecond, 5)}
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTables
}

// Validate checks the token is Airtable's: /meta/whoami answers.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	var who struct {
		ID string `json:"id"`
	}
	if err := p.get(ctx, tok, "/meta/whoami", nil, &who); err != nil {
		return err
	}
	if who.ID == "" {
		return errors.New("airtable /meta/whoami answered no user")
	}
	return nil
}

// base is one base the token reaches.
type base struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	PermissionLevel string `json:"permissionLevel"`
}

// bases lists the bases the token reaches.
func (p *Provider) bases(ctx context.Context, tok string) ([]base, error) {
	var out []base
	offset := ""
	for len(out) < maxBases {
		q := url.Values{}
		if offset != "" {
			q.Set("offset", offset)
		}
		var page struct {
			Bases  []base `json:"bases"`
			Offset string `json:"offset"`
		}
		if err := p.get(ctx, tok, "/meta/bases", q, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Bases...)
		if page.Offset == "" {
			break
		}
		offset = page.Offset
	}
	return out, nil
}

// Discover lists the bases the token reaches, for the "pick bases" list.
// The pick comes back as options.bases; none brings every base.
func (p *Provider) Discover(ctx context.Context, ownerUserID string, token *importModels.Token) ([]importProvider.DiscoverItem, error) {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return nil, errors.New("no Airtable token saved")
	}
	bs, err := p.bases(ctx, token.AccessToken)
	if err != nil {
		return nil, err
	}
	out := make([]importProvider.DiscoverItem, 0, len(bs))
	for _, b := range bs {
		out = append(out, importProvider.DiscoverItem{ID: b.ID, Name: b.Name, Kind: "base", Meta: map[string]any{"permission": b.PermissionLevel}})
	}
	return out, nil
}

// chosenBases is options.bases, as ids.
func chosenBases(opts importProvider.JobOptions) map[string]bool {
	out := map[string]bool{}
	switch v := opts["bases"].(type) {
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && strings.TrimSpace(s) != "" {
				out[strings.TrimSpace(s)] = true
			}
		}
	case []string:
		for _, s := range v {
			if strings.TrimSpace(s) != "" {
				out[strings.TrimSpace(s)] = true
			}
		}
	}
	return out
}

// Plan queues a table chunk per base and counts its tables.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	bs, err := p.bases(ctx, tok)
	if err != nil {
		return nil, nil, err
	}
	want := chosenBases(opts)
	plan := &importProvider.Plan{}
	var chunks []*importModels.Chunk
	for _, b := range bs {
		if len(want) > 0 && !want[b.ID] {
			continue
		}
		schema, err := p.schema(ctx, tok, b.ID)
		if err != nil {
			return nil, nil, err
		}
		plan.TableCount += len(schema.Tables)
		id := b.ID
		chunks = append(chunks, &importModels.Chunk{ImportId: j.Id, ChunkType: importModels.ChunkTableBase, ParentSourceId: &id, Status: importModels.ChunkStatusPending})
	}
	if len(chunks) == 0 {
		plan.Warnings = append(plan.Warnings, "The token reaches no base to bring. Give it access to the bases, with the data.records:read and schema.bases:read scopes.")
	}
	return plan, chunks, nil
}

// schemaOf is a base's tables, as /meta/bases/{id}/tables gives them.
type schemaOf struct {
	Tables []atTable `json:"tables"`
}

type atTable struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	PrimaryFieldID string    `json:"primaryFieldId"`
	Fields         []atField `json:"fields"`
	Views          []atView  `json:"views"`
}

type atField struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Options     json.RawMessage `json:"options"`
}

type atView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func (p *Provider) schema(ctx context.Context, tok, baseID string) (*schemaOf, error) {
	var s schemaOf
	if err := p.get(ctx, tok, "/meta/bases/"+url.PathEscape(baseID)+"/tables", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// BaseOf reads a base: its tables and fields now, its records as the
// importer asks for them, a page at a time.
func (p *Provider) BaseOf(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, baseID string, read func(rows int)) (dataTableBusiness.ImportBase, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return dataTableBusiness.ImportBase{}, err
	}
	s, err := p.schema(ctx, tok, baseID)
	if err != nil {
		return dataTableBusiness.ImportBase{}, err
	}
	m := newMapping(s)
	out := dataTableBusiness.ImportBase{}
	rows := 0
	for _, t := range s.Tables {
		t := t
		it := m.table(t)
		for field, note := range m.Notes(t) {
			out.Notes = append(out.Notes, dataTableBusiness.ImportNote{Table: t.ID, Field: field, Code: "FIELD_AS_LESS", Msg: note})
		}
		it.Rows = func(page func([]dataTableBusiness.ImportRow) error) error {
			return p.records(ctx, tok, baseID, t.ID, func(recs []atRecord) error {
				rows += len(recs)
				if read != nil {
					read(rows)
				}
				return page(m.rows(t, recs))
			})
		}
		out.Tables = append(out.Tables, it)
	}
	return out, nil
}

// atRecord is one record, its cells by field id.
type atRecord struct {
	ID          string                     `json:"id"`
	CreatedTime string                     `json:"createdTime"`
	Fields      map[string]json.RawMessage `json:"fields"`
}

// records reads a table's records, a page at a time, in the table's order.
func (p *Provider) records(ctx context.Context, tok, baseID, tableID string, each func([]atRecord) error) error {
	offset := ""
	for {
		q := url.Values{}
		q.Set("pageSize", strconv.Itoa(pageSize))
		q.Set("returnFieldsByFieldId", "true")
		q.Set("cellFormat", "json")
		if offset != "" {
			q.Set("offset", offset)
		}
		var page struct {
			Records []atRecord `json:"records"`
			Offset  string     `json:"offset"`
		}
		if err := p.get(ctx, tok, "/"+url.PathEscape(baseID)+"/"+url.PathEscape(tableID), q, &page); err != nil {
			return err
		}
		if len(page.Records) > 0 {
			if err := each(page.Records); err != nil {
				return err
			}
		}
		if page.Offset == "" {
			return nil
		}
		offset = page.Offset
	}
}

// FetchAttachment downloads a file by its address, which Airtable signs for
// a few hours: no token goes with it.
func (p *Provider) FetchAttachment(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, att importProvider.SourceAttachment, dest io.Writer) (string, int64, error) {
	return importProvider.DefaultFetchAttachment(ctx, att, dest)
}

// ─── HTTP ───────────────────────────────────────────────────────────

// statusRules reads Airtable's answers. Only a 401 refuses the token: a base
// it can't reach answers 404 or 403 NOT_AUTHORIZED. Airtable asks for 30
// seconds after a 429.
var statusRules = importProvider.StatusRules{Name: "Airtable", Wait: 30 * time.Second, OnlyUnauthorized: true}

func (p *Provider) get(ctx context.Context, tok, path string, q url.Values, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	u := apiBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	return statusRules.SendJSON(req, out)
}

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j == nil || j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	t, err := importModels.LoadToken(ctx, providerName, *j.TriggeredBy)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(t.AccessToken) == "" {
		return "", errors.New("no Airtable token saved; connect Airtable first")
	}
	return t.AccessToken, nil
}
