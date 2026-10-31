package gsheets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// fakeSheets is the Sheets API holding one spreadsheet, ss1: Deals, a sheet
// of 1,500 rows whose rows 1001 on come in a second read, a hidden Lists
// sheet and a chart's sheet. Every read is counted by its range.
func fakeSheets(t *testing.T, reads map[string]int) *httptest.Server {
	const dropdown = `"dataValidation":{"condition":{"type":"ONE_OF_LIST","values":[{"userEnteredValue":"Lead"},{"userEnteredValue":"Won"},{"userEnteredValue":" Lost "}]}}`
	const usd = `"effectiveFormat":{"numberFormat":{"type":"CURRENCY","pattern":"\"$\"#,##0.00"}}`
	const day = `"effectiveFormat":{"numberFormat":{"type":"DATE","pattern":"dd/mm/yyyy"}}`
	const pct = `"effectiveFormat":{"numberFormat":{"type":"PERCENT"}}`
	text := func(s string) string {
		return fmt.Sprintf(`{"formattedValue":%q,"effectiveValue":{"stringValue":%q}}`, s, s)
	}
	chip := func(name, email string) string {
		return fmt.Sprintf(`{"formattedValue":%q,"effectiveValue":{"stringValue":%q},"chipRuns":[{"chip":{"personProperties":{"email":%q}}}]}`, name, name, email)
	}
	row := func(cells ...string) string { return `{"values":[` + strings.Join(cells, ",") + `]}` }
	pages := map[string]string{
		"'Deals'!1:1000": strings.Join([]string{
			row(text("Deal"), text("Stage"), text("Owner"), text("Amount"), text("Closes"), text("Spec"), text("Won"), text("Share")),
			row(text("Booth"), `{"formattedValue":"Won","effectiveValue":{"stringValue":"Won"},`+dropdown+`}`, chip("Ann Lee", "ann@example.com"),
				`{"formattedValue":"$1,200.50","effectiveValue":{"numberValue":1200.5},`+usd+`}`,
				`{"formattedValue":"23/10/2026","effectiveValue":{"numberValue":46318},`+day+`}`,
				`{"formattedValue":"Spec","effectiveValue":{"stringValue":"Spec"},"hyperlink":"https://example.com/spec"}`,
				`{"formattedValue":"TRUE","effectiveValue":{"boolValue":true}}`,
				`{"formattedValue":"12.5%","effectiveValue":{"numberValue":0.125},`+pct+`}`),
			`{}`,
			row(text("Banners"), `{`+dropdown+`}`, chip("Bo", "bo@example.com"),
				`{"formattedValue":"$30.00","effectiveValue":{"numberValue":30},`+usd+`}`,
				`{"formattedValue":"01/01/1900","effectiveValue":{"numberValue":2},`+day+`}`,
				`{"formattedValue":"https://example.com/b","effectiveValue":{"stringValue":"https://example.com/b"},"hyperlink":"https://example.com/b"}`,
				`{"formattedValue":"FALSE","effectiveValue":{"boolValue":false}}`,
				`{"formattedValue":"7%","effectiveValue":{"numberValue":0.07},`+pct+`}`, `{"effectiveFormat":{}}`),
		}, ","),
		"'Deals'!1001:2000": row(text("Cups"), `{"formattedValue":"Lost","effectiveValue":{"stringValue":"Lost"},`+dropdown+`}`, chip("Cy", "cy@example.com"),
			`{"formattedValue":"$5.00","effectiveValue":{"numberValue":5},"userEnteredValue":{"formulaValue":"=2+3"},`+usd+`}`,
			`{"formattedValue":"05/01/2026","effectiveValue":{"numberValue":46027},`+day+`}`,
			`{"formattedValue":"C","effectiveValue":{"stringValue":"C"},"hyperlink":"https://example.com/c"}`,
			`{"formattedValue":"TRUE","effectiveValue":{"boolValue":true}}`,
			`{"formattedValue":"50%","effectiveValue":{"numberValue":0.5},`+pct+`}`),
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/spreadsheets/ss1" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		q := r.URL.Query()
		if q.Get("includeGridData") != "true" {
			_, _ = w.Write([]byte(`{"properties":{"locale":"en_GB","timeZone":"Asia/Kolkata"},"sheets":[
				{"properties":{"title":"Deals","sheetType":"GRID","gridProperties":{"rowCount":1500}}},
				{"properties":{"title":"Lists","hidden":true,"sheetType":"GRID","gridProperties":{"rowCount":10}}},
				{"properties":{"title":"Chart 1","sheetType":"OBJECT"}}]}`))
			return
		}
		rng := q.Get("ranges")
		reads[rng]++
		page, ok := pages[rng]
		if !ok {
			t.Errorf("a read of %s", rng)
		}
		_, _ = w.Write([]byte(`{"sheets":[{"data":[{"rowData":[` + page + `]}]}]}`))
	}))
}

func testProvider(t *testing.T, reads map[string]int) *Provider {
	srv := fakeSheets(t, reads)
	t.Cleanup(srv.Close)
	was := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = was })
	return &Provider{rl: importProvider.NewSleepLimiter(time.Millisecond, 100)}
}

// A spreadsheet comes across as its sheets say: a table of each sheet of
// cells that isn't hidden, each column the kind its cells say (a dropdown a
// select, chips people, links urls, typed amounts, days, checkboxes and
// percents), its rows read a page at a time to the grid's last row, past an
// empty row and an empty page end, and what doesn't come whole said.
func TestASpreadsheetComesAcrossAsItsSheetsSay(t *testing.T) {
	reads := map[string]int{}
	p := testProvider(t, reads)
	var counted []int
	base, err := p.base(context.Background(), "tok1", "ss1", func(rows int) { counted = append(counted, rows) })
	if err != nil {
		t.Fatal(err)
	}
	if base.Private || len(base.Tables) != 1 || base.Tables[0].Name != "Deals" {
		t.Fatalf("base: private %v, %d tables", base.Private, len(base.Tables))
	}
	tb := base.Tables[0]
	var kinds []string
	for _, f := range tb.Fields {
		kinds = append(kinds, f.Name+":"+f.Type)
	}
	want := []string{"Deal:" + model.FieldText, "Stage:" + model.FieldSelect, "Owner:" + model.FieldPeople, "Amount:" + model.FieldNumber,
		"Closes:" + model.FieldDate, "Spec:" + model.FieldURL, "Won:" + model.FieldCheckbox, "Share:" + model.FieldNumber}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("fields %v, want %v", kinds, want)
	}
	var options []string
	for _, o := range tb.Fields[1].Config["options"].([]any) {
		options = append(options, fmt.Sprint(o.(map[string]any)["label"]))
	}
	if fmt.Sprint(options) != "[Lead Won Lost]" || fmt.Sprint(tb.Fields[3].Config) != "map[format:map[currency:USD]]" ||
		fmt.Sprint(tb.Fields[7].Config) != "map[format:map[percent:true]]" {
		t.Errorf("configs: stage %v, amount %v, share %v", options, tb.Fields[3].Config, tb.Fields[7].Config)
	}
	var rows []dataTableBusiness.ImportRow
	if err := tb.Rows(func(page []dataTableBusiness.ImportRow) error { rows = append(rows, page...); return nil }); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range rows {
		var cells []string
		for _, f := range tb.Fields {
			cells = append(cells, fmt.Sprint(r.Cells[f.Key]))
		}
		got = append(got, strings.Join(cells, "|"))
	}
	wantRows := []string{
		"Booth|Won|[{ann@example.com }]|1200.5|2026-10-23|https://example.com/spec|true|12.5",
		"Banners|<nil>|[{bo@example.com }]|30|1900-01-01|https://example.com/b|<nil>|7",
		"Cups|Lost|[{cy@example.com }]|5|2026-01-05|https://example.com/c|true|50",
	}
	if fmt.Sprint(got) != fmt.Sprint(wantRows) {
		t.Errorf("rows:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(wantRows, "\n"))
	}
	if reads["'Deals'!1:1000"] != 2 || reads["'Deals'!1001:2000"] != 2 || len(reads) != 2 {
		t.Errorf("reads %v: want each page of Deals read by the guess and the import, and nothing else", reads)
	}
	if len(counted) == 0 || counted[len(counted)-1] != 3 {
		t.Errorf("rows counted %v, want 3 at the end", counted)
	}
	var notes []string
	for _, n := range base.Notes {
		notes = append(notes, n.Table+":"+n.Code)
	}
	if fmt.Sprint(notes) != "[Deals:FORMULA_AS_VALUES Deals:LINK_TEXT_LEFT_OUT Lists:SHEET_LEFT_OUT]" {
		t.Errorf("notes %v", notes)
	}
}

// The plan counts the sheets that come across, a chunk a spreadsheet, and
// a token Google refuses is said to be refused.
func TestASpreadsheetsPlanAndARefusedToken(t *testing.T) {
	p := testProvider(t, map[string]int{})
	job := uuid.New()
	plan, chunks, err := p.plan(context.Background(), "tok1", job, []string{"ss1"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.TableCount != 1 || len(chunks) != 1 || *chunks[0].ParentSourceId != "ss1" || chunks[0].ImportId != job || len(plan.Warnings) != 0 {
		t.Errorf("plan %+v, chunks %d", plan, len(chunks))
	}
	if plan, chunks, err := p.plan(context.Background(), "tok1", job, nil); err != nil || len(chunks) != 0 || len(plan.Warnings) != 1 {
		t.Errorf("no spreadsheet: %v, %d chunks, warnings %v", err, len(chunks), plan.Warnings)
	}
	if _, err := p.spreadsheet(context.Background(), "wrong", "ss1"); !errors.Is(err, importProvider.ErrTokenRejected) {
		t.Errorf("a wrong token: %v", err)
	}
	if _, err := p.spreadsheet(context.Background(), "tok1", "gone"); err == nil || errors.Is(err, importProvider.ErrTokenRejected) || !strings.Contains(err.Error(), "spreadsheet gone") {
		t.Errorf("a spreadsheet the token can't reach: %v", err)
	}
}

// A spreadsheet is named by its id or its address, each once; anything else
// is refused, and so are more spreadsheets than one import brings.
func TestSpreadsheetsAreNamedByIdOrAddress(t *testing.T) {
	ids, err := chosen(importProvider.JobOptions{"spreadsheets": []any{
		"https://docs.google.com/spreadsheets/d/abc_DEF-123/edit#gid=0", " abc_DEF-123 ", "https://docs.google.com/spreadsheets/u/1/d/xyz/edit", "xyz"}})
	if err != nil || fmt.Sprint(ids) != "[abc_DEF-123 xyz]" {
		t.Errorf("ids %v, %v", ids, err)
	}
	if ids, err := chosen(importProvider.JobOptions{}); err != nil || len(ids) != 0 {
		t.Errorf("none named: %v, %v", ids, err)
	}
	many := make([]string, maxSpreadsheets+1)
	for i := range many {
		many[i] = fmt.Sprintf("s%d", i)
	}
	for name, v := range map[string]any{
		"another site's address": []any{"https://example.com/spreadsheets/d/abc"},
		"a path":                 []any{"../x"},
		"a number":               []any{5},
		"too many":               many,
	} {
		if _, err := chosen(importProvider.JobOptions{"spreadsheets": v}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
