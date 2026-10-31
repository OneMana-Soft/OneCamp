// Package gsheets implements the Import Provider interface for Google Sheets
// (tables S1 slice 6, gsheets-import-01): each spreadsheet an admin names
// comes across as a base, a table a sheet, through the tables stage
// (CapTables, business/Import tables_worker.go) and the one importer of every
// source's tables (business/DataTable ImportBaseAs), as Airtable's bases do.
//
// Source: live API only ("api"), the Sheets API v4.
//
// Auth: the admin signs in with the server's Google client, the one Google
// sign-in and the connectors borrow (oauth.GoogleClientCreds), asking only to
// read spreadsheets. The token is kept and refreshed as Basecamp's is
// (importProvider.FreshAccessToken).
//
// What comes across, per spreadsheet options.spreadsheets names (by id or
// address): every sheet of cells that isn't hidden, its first row with a
// value naming its columns, each column's kind guessed by the tables' sheet
// guess (dataTableBusiness.SheetsBase) from what the sheet says of its cells:
// numbers, days and times typed by their formats, checkboxes, dropdowns as
// selects, people chips as people (matched to members by email) and links as
// urls. Formulas come as the values they last worked out, which is said.
package gsheets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	formula "github.com/akashc777/OneCamp/business/Formula"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/initializers/oauth"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const providerName = importModels.ProviderGoogleSheets

// apiBase is the Sheets API, a variable for tests.
var apiBase = "https://sheets.googleapis.com/v4"

const (
	// rowsPerRead is how many of a sheet's rows one read asks for.
	rowsPerRead = 1000
	// maxSpreadsheets is how many spreadsheets one import brings.
	maxSpreadsheets = 100
)

// Provider implements importProvider.Provider and TableSource for Google
// Sheets.
type Provider struct {
	// A spreadsheet brings tables only.
	importProvider.NoTasks

	rl importProvider.Limiter
}

// New constructs a provider with its own limiter: a read a second, the
// Sheets API's 60 a minute for one person.
func New() *Provider {
	return &Provider{rl: importProvider.NewSleepLimiter(time.Second, 1)}
}

func init() { importProvider.Register(New()) }

func (p *Provider) Name() string               { return providerName }
func (p *Provider) SupportedSources() []string { return []string{importModels.SourceAPI} }
func (p *Provider) Capabilities() importProvider.Capability {
	return importProvider.CapTables
}

// ─── Signing in ──────────────────────────────────────────────────────

func oauthConfig() importProvider.OAuthConfig {
	id, secret := oauth.GoogleClientCreds()
	return importProvider.OAuthConfig{ClientID: id, ClientSecret: secret, Endpoint: oauth.GoogleEndpoint}
}

// OAuthConnect is how an admin signs in to Google: offline, so the token is
// refreshed as an import runs, and with the consent screen each time, so
// Google sends the refresh token again.
func (p *Provider) OAuthConnect() importProvider.OAuthConnect {
	return importProvider.OAuthConnect{
		Config:     oauthConfig(),
		Scopes:     []string{"https://www.googleapis.com/auth/spreadsheets.readonly"},
		AuthParams: map[string]string{"access_type": "offline", "prompt": "consent"},
		Setup: "Signing in to Google isn't set up on this server. Set up Google sign-in with its client id and secret, " +
			"add https://<this server's API address>/admin/import/oauth/callback to that client's redirect URIs, " +
			"and turn on the Google Sheets API in its Google Cloud project.",
	}
}

func (p *Provider) loadToken(ctx context.Context, j *importModels.Job) (string, error) {
	if j == nil || j.TriggeredBy == nil {
		return "", errors.New("job has no triggered_by user")
	}
	return importProvider.FreshAccessToken(ctx, oauthConfig(), providerName, *j.TriggeredBy)
}

// ─── Which spreadsheets ──────────────────────────────────────────────

var (
	// spreadsheetAddress is a spreadsheet's address, its id after /d/.
	spreadsheetAddress = regexp.MustCompile(`^https://docs\.google\.com/spreadsheets/(?:u/\d+/)?d/([A-Za-z0-9_-]+)`)
	spreadsheetID      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,100}$`)
)

// chosen is options.spreadsheets as ids, in order and each once: each an id
// or a spreadsheet's address.
func chosen(opts importProvider.JobOptions) ([]string, error) {
	var named []string
	switch v := opts["spreadsheets"].(type) {
	case []string:
		named = v
	case []any:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("a spreadsheet is named by its id or address, not %v", x)
			}
			named = append(named, s)
		}
	}
	var ids []string
	seen := map[string]bool{}
	for _, s := range named {
		s = strings.TrimSpace(s)
		if m := spreadsheetAddress.FindStringSubmatch(s); m != nil {
			s = m[1]
		}
		if !spreadsheetID.MatchString(s) {
			return nil, fmt.Errorf("%q isn't a Google Sheets spreadsheet's id or address", s)
		}
		if !seen[s] {
			seen[s] = true
			ids = append(ids, s)
		}
	}
	if len(ids) > maxSpreadsheets {
		return nil, fmt.Errorf("an import brings at most %d spreadsheets; this names %d", maxSpreadsheets, len(ids))
	}
	return ids, nil
}

// Validate checks the job names spreadsheets, and the token reads the first.
func (p *Provider) Validate(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) error {
	ids, err := chosen(opts)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return errors.New("name the Google Sheets spreadsheets to bring, by their addresses")
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return err
	}
	_, err = p.spreadsheet(ctx, tok, ids[0])
	return err
}

// Plan queues a table chunk per spreadsheet and counts the sheets to bring.
func (p *Provider) Plan(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions) (*importProvider.Plan, []*importModels.Chunk, error) {
	ids, err := chosen(opts)
	if err != nil {
		return nil, nil, err
	}
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, nil, err
	}
	return p.plan(ctx, tok, j.Id, ids)
}

func (p *Provider) plan(ctx context.Context, tok string, job uuid.UUID, ids []string) (*importProvider.Plan, []*importModels.Chunk, error) {
	plan := &importProvider.Plan{}
	var chunks []*importModels.Chunk
	for _, id := range ids {
		m, err := p.spreadsheet(ctx, tok, id)
		if err != nil {
			return nil, nil, err
		}
		for _, s := range m.Sheets {
			if s.Properties.SheetType == "GRID" && !s.Properties.Hidden {
				plan.TableCount++
			}
		}
		chunks = append(chunks, &importModels.Chunk{ImportId: job, ChunkType: importModels.ChunkTableBase, ParentSourceId: &id, Status: importModels.ChunkStatusPending})
	}
	if len(chunks) == 0 {
		plan.Warnings = append(plan.Warnings, "No spreadsheet is named. Name the spreadsheets to bring by their addresses.")
	}
	return plan, chunks, nil
}

// ─── A spreadsheet ───────────────────────────────────────────────────

// meta is a spreadsheet's metadata: its locale and time zone, which say how
// its text reads, and its sheets.
type meta struct {
	Properties struct {
		Locale   string `json:"locale"`
		TimeZone string `json:"timeZone"`
	} `json:"properties"`
	Sheets []struct {
		Properties struct {
			Title  string `json:"title"`
			Hidden bool   `json:"hidden"`
			// SheetType is GRID for a sheet of cells, OBJECT for a chart's.
			SheetType      string `json:"sheetType"`
			GridProperties struct {
				RowCount int `json:"rowCount"`
			} `json:"gridProperties"`
		} `json:"properties"`
	} `json:"sheets"`
}

const metaFields = "properties(locale,timeZone),sheets(properties(title,hidden,sheetType,gridProperties(rowCount)))"

func (p *Provider) spreadsheet(ctx context.Context, tok, id string) (*meta, error) {
	var m meta
	if err := p.get(ctx, tok, "/spreadsheets/"+url.PathEscape(id), url.Values{"fields": {metaFields}}, &m); err != nil {
		return nil, fmt.Errorf("spreadsheet %s: %w", id, err)
	}
	return &m, nil
}

// BaseOf reads a spreadsheet as a base: a table of each sheet of cells that
// isn't hidden, its rows read from the API as the importer asks for them.
func (p *Provider) BaseOf(ctx context.Context, j *importModels.Job, opts importProvider.JobOptions, id string, read func(rows int)) (dataTableBusiness.ImportBase, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return dataTableBusiness.ImportBase{}, err
	}
	return p.base(ctx, tok, id, read)
}

func (p *Provider) base(ctx context.Context, tok, id string, read func(rows int)) (dataTableBusiness.ImportBase, error) {
	m, err := p.spreadsheet(ctx, tok, id)
	if err != nil {
		return dataTableBusiness.ImportBase{}, err
	}
	var sheets []contentIOBusiness.Sheet
	for _, s := range m.Sheets {
		props := s.Properties
		if props.SheetType != "GRID" {
			continue
		}
		sheets = append(sheets, contentIOBusiness.Sheet{Name: props.Title, Hidden: props.Hidden, Rows: func(row func([]contentIOBusiness.Cell) error) error {
			return p.rows(ctx, tok, id, props.Title, props.GridProperties.RowCount, row)
		}})
	}
	zone, err := time.LoadLocation(m.Properties.TimeZone)
	if err != nil {
		zone = time.UTC
	}
	// A typed cell says what it holds; the locale only says how a text cell
	// that looks like a day reads (03/04/2026 is 4 March in en_US).
	opts := dataTableBusiness.SheetOptions{Signs: formula.PlainSigns, DayFirst: m.Properties.Locale != "en_US", Zone: zone}
	return dataTableBusiness.SheetsBase(sheets, opts, func(done, _ int) {
		if read != nil {
			read(done)
		}
	})
}

// gridPage is one read of a sheet's rows.
type gridPage struct {
	Sheets []struct {
		Data []struct {
			RowData []struct {
				Values []gridCell `json:"values"`
			} `json:"rowData"`
		} `json:"data"`
	} `json:"sheets"`
}

// gridCell is a cell as the API gives it.
type gridCell struct {
	FormattedValue string `json:"formattedValue"`
	EffectiveValue *struct {
		NumberValue *float64  `json:"numberValue"`
		BoolValue   *bool     `json:"boolValue"`
		ErrorValue  *struct{} `json:"errorValue"`
	} `json:"effectiveValue"`
	UserEnteredValue *struct {
		FormulaValue string `json:"formulaValue"`
	} `json:"userEnteredValue"`
	EffectiveFormat *struct {
		NumberFormat struct {
			Type    string `json:"type"`
			Pattern string `json:"pattern"`
		} `json:"numberFormat"`
	} `json:"effectiveFormat"`
	DataValidation *struct {
		Condition struct {
			Type   string `json:"type"`
			Values []struct {
				UserEnteredValue string `json:"userEnteredValue"`
			} `json:"values"`
		} `json:"condition"`
	} `json:"dataValidation"`
	Hyperlink string `json:"hyperlink"`
	ChipRuns  []struct {
		Chip struct {
			PersonProperties *struct {
				Email string `json:"email"`
			} `json:"personProperties"`
			RichLinkProperties *struct {
				URI string `json:"uri"`
			} `json:"richLinkProperties"`
		} `json:"chip"`
	} `json:"chipRuns"`
}

const gridFields = "sheets(data(rowData(values(formattedValue,effectiveValue,userEnteredValue/formulaValue," +
	"effectiveFormat/numberFormat,dataValidation/condition,hyperlink,chipRuns(chip(personProperties/email,richLinkProperties/uri))))))"

// rows reads a sheet from its first row to its last (rowCount, its grid's
// size: the API leaves out a range's empty rows at its end, so a short read
// isn't the sheet's end), rowsPerRead at a time, handing row the cells of
// each row with a value, its empty cells at the end left off. The guess
// reads a sheet once and the import again, so a sheet is read twice.
func (p *Provider) rows(ctx context.Context, tok, id, title string, rowCount int, row func([]contentIOBusiness.Cell) error) error {
	sheet := "'" + strings.ReplaceAll(title, "'", "''") + "'"
	for from := 1; from <= rowCount; from += rowsPerRead {
		q := url.Values{
			"ranges":          {fmt.Sprintf("%s!%d:%d", sheet, from, from+rowsPerRead-1)},
			"includeGridData": {"true"},
			"fields":          {gridFields},
		}
		var page gridPage
		if err := p.get(ctx, tok, "/spreadsheets/"+url.PathEscape(id), q, &page); err != nil {
			return err
		}
		for _, s := range page.Sheets {
			for _, d := range s.Data {
				for _, r := range d.RowData {
					cells := make([]contentIOBusiness.Cell, len(r.Values))
					last, valued := -1, false
					for i, g := range r.Values {
						cells[i] = cellOf(g)
						// An empty cell's dropdown still says its column's
						// kind, so it isn't left off the row's end.
						if v := cells[i].Text != "" || cells[i].Kind != contentIOBusiness.CellText; v || cells[i].Choices != nil {
							last, valued = i, valued || v
						}
					}
					if !valued {
						continue
					}
					if err := row(cells[:last+1]); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// cellOf is a cell as the sheet says it: its value typed by its number
// format, the text it shows, and its dropdown's values, link and people.
func cellOf(g gridCell) contentIOBusiness.Cell {
	var c contentIOBusiness.Cell
	switch v := g.EffectiveValue; {
	case v == nil:
	case v.ErrorValue != nil:
		c.Kind = contentIOBusiness.CellError
	case v.BoolValue != nil:
		c.Kind, c.Bool = contentIOBusiness.CellBool, *v.BoolValue
	case v.NumberValue != nil:
		c = contentIOBusiness.NumberCell(*v.NumberValue, formatCode(g))
	}
	c.Text = g.FormattedValue
	c.Formula = g.UserEnteredValue != nil && g.UserEnteredValue.FormulaValue != ""
	if d := g.DataValidation; d != nil && d.Condition.Type == "ONE_OF_LIST" {
		for _, x := range d.Condition.Values {
			if s := strings.TrimSpace(x.UserEnteredValue); s != "" {
				c.Choices = append(c.Choices, s)
			}
		}
	}
	c.Link = g.Hyperlink
	for _, r := range g.ChipRuns {
		switch ch := r.Chip; {
		case ch.PersonProperties != nil && ch.PersonProperties.Email != "":
			c.People = append(c.People, ch.PersonProperties.Email)
		case ch.RichLinkProperties != nil && c.Link == "":
			c.Link = ch.RichLinkProperties.URI
		}
	}
	return c
}

// formatCode is a number's format code: the cell's pattern, or for a format
// the spreadsheet's locale writes out, one of its type's.
func formatCode(g gridCell) string {
	if g.EffectiveFormat == nil {
		return "General"
	}
	f := g.EffectiveFormat.NumberFormat
	if f.Pattern != "" {
		return f.Pattern
	}
	switch f.Type {
	case "DATE":
		return "yyyy-mm-dd"
	case "DATE_TIME":
		return "yyyy-mm-dd hh:mm:ss"
	case "TIME":
		return "hh:mm:ss"
	case "PERCENT":
		return "0%"
	}
	return "General"
}

// FetchAttachment: a spreadsheet's cells bring no files.
func (p *Provider) FetchAttachment(context.Context, *importModels.Job, importProvider.JobOptions, importProvider.SourceAttachment, io.Writer) (string, int64, error) {
	return "", 0, errors.New("a Google Sheets import brings no files")
}

// ─── HTTP ────────────────────────────────────────────────────────────

// statusRules reads Google's answers. Only a 401 refuses the token: a
// spreadsheet it can't reach answers 403 or 404. Google's read quota is per
// minute, so a 429 that names no wait holds the import a minute.
var statusRules = importProvider.StatusRules{Name: "Google Sheets", Wait: time.Minute, OnlyUnauthorized: true}

func (p *Provider) get(ctx context.Context, tok, path string, q url.Values, out any) error {
	if err := p.rl.Wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	return statusRules.SendJSON(req, out)
}
