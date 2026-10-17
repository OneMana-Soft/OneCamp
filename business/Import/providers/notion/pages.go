package notion

// Notion pages into docs (docs programme C4; structure.md P2.4). With the
// import's "pages" option, every page shared with the connection comes across
// as a doc, in its tree, with its title and icon, and a database inside a page
// becomes a table, shown where it stood as a table embed. The import's pages
// stage makes the docs (business/Import pages_worker.go, through the docs
// pipeline). A database becomes a table only through TableFromDatabase
// (business/ContentIO/adapters/notion), which the tables and projects
// importers call too.
//
// THE API, as Notion documents it (developers.notion.com, read 14 Oct 2026):
//   - Pages are read at Notion-Version 2026-03-11, the current one ("Changes
//     by version"); the task import keeps 2022-06-28 (provider.go). Since
//     2025-09-03 a database's rows live in its data sources: GET
//     /v1/databases/{id} names them (data_sources[].id), GET
//     /v1/data_sources/{id} has the columns (properties), and POST
//     /v1/data_sources/{id}/query the rows. A database with several data
//     sources becomes a table for each.
//   - POST /v1/search with filter {property: object, value: page} lists the
//     pages shared with the connection, each with its parent: the workspace,
//     a page (page_id), a block (block_id: a page inside a column or a
//     toggle), or a data source (a database's row, which comes across in the
//     table, not as a doc).
//   - GET /v1/pages/{id} has a page's title (its "title" property), its icon
//     and in_trash; GET /v1/blocks/{id}/children its blocks, a page or a
//     database under it as a child_page or child_database block, whose id is
//     the page's or the database's own. A duplicate synced block's content is
//     its original's (synced_from.block_id).
//   - A file Notion keeps is a signed address that expires within the hour,
//     so a picture is fetched while its page is read (through SafeFetch) and
//     goes in with the page. A picture elsewhere on the web keeps its address.
//     Other files stay in Notion, and the import says so.

import (
	"context"
	"errors"
	"fmt"
	"html"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	notionAdapter "github.com/akashc777/OneCamp/business/ContentIO/adapters/notion"
	dataTableBusiness "github.com/akashc777/OneCamp/business/DataTable"
	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
)

const (
	// pagesVersion is the Notion-Version pages are read at.
	pagesVersion = "2026-03-11"
	// maxBlockDepth is how deep blocks inside blocks are read.
	maxBlockDepth = 12
	// maxTableRows is the most rows a data source brings to its table.
	maxTableRows = 2000
	// maxPictureBytes and pictureTimeout bound reading one picture.
	maxPictureBytes = 20 << 20
	pictureTimeout  = 30 * time.Second
)

// maxTreePages is the most pages one tree brings.
var maxTreePages = 1000

// Seams for tests: where a picture is read from, how a database becomes a
// table, the import's id map, and where a note to the importer goes.
var (
	fetchPicture = func(ctx context.Context, address string) (*safefetch.Response, error) {
		return safefetch.Fetch(ctx, address, safefetch.Options{MaxBytes: maxPictureBytes, Timeout: pictureTimeout, Accept: "image/*"})
	}
	tableFromDatabase = notionAdapter.TableFromDatabase
	lookupMapped      = importModels.LookupIdMapping
	recordMapped      = importModels.UpsertIdMappingWithOwnership
	noteImport        = importModels.LogImportError
)

// pagesOn is whether an import brings pages (its "pages" option).
func pagesOn(opts importProvider.JobOptions) bool {
	on, _ := opts["pages"].(bool)
	return on
}

// pageKey is a page's id as the import names it: 32 hex digits, without the
// dashes the API writes, as a link to the page in Notion writes it.
func pageKey(id string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(id)), "-", "")
}

// pagesWarnings are what a pages import leaves in Notion, for the plan.
var pagesWarnings = []string{
	"Pages come across as docs private to the person importing, in their tree; share them or move them into a space afterwards.",
	"A database's rows come across in its table; what is written inside a row's own page stays in Notion.",
	"Comments on pages, and files other than pictures, stay in Notion.",
}

// searchHit is a page as /search lists it.
type searchHit struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	InTrash bool         `json:"in_trash"`
	Parent  notionParent `json:"parent"`
}

// planPages finds the trees of pages shared with the connection, and how many
// pages they hold: a page under no other page the connection can read is a
// tree's root. A page sitting in a block (a column, a toggle) is taken for a
// root too, as finding the page the block is in costs a request or more for
// each and planning answers while the admin waits; its chunk finds out
// (PagesOf), and brings nothing when another tree brings it. more says the
// connection shares more pages than an import reads.
func (p *Provider) planPages(ctx context.Context, tok string) (roots []string, pages int, more bool, err error) {
	shared := map[string]notionParent{}
	cursor := ""
	for i := 0; ; i++ {
		if i == maxPages {
			more = true
			break
		}
		body := map[string]any{"filter": map[string]any{"property": "object", "value": "page"}, "page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		var resp struct {
			Results    []searchHit `json:"results"`
			HasMore    bool        `json:"has_more"`
			NextCursor string      `json:"next_cursor"`
		}
		if err := p.call(ctx, tok, pagesVersion, http.MethodPost, "/search", body, &resp); err != nil {
			return nil, 0, false, err
		}
		for _, h := range resp.Results {
			if h.Object != "page" || h.InTrash || h.Parent.Type == "data_source_id" || h.Parent.Type == "database_id" {
				continue
			}
			shared[pageKey(h.ID)] = h.Parent
		}
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	for key, parent := range shared {
		if _, under := shared[pageKey(parent.PageID)]; parent.Type != "page_id" || !under {
			roots = append(roots, key)
		}
	}
	sort.Strings(roots)
	return roots, len(shared), more, nil
}

// pageOfBlock is the page a block sits in, up through the blocks above it;
// "" when that can't be read.
func (p *Provider) pageOfBlock(ctx context.Context, tok, blockID string) string {
	for i := 0; i < maxBlockDepth && blockID != ""; i++ {
		var b notionBlock
		if err := p.call(ctx, tok, pagesVersion, http.MethodGet, "/blocks/"+url.PathEscape(blockID), nil, &b); err != nil || b.Parent == nil {
			return ""
		}
		switch b.Parent.Type {
		case "page_id":
			return pageKey(b.Parent.PageID)
		case "block_id":
			blockID = b.Parent.BlockID
		default:
			return ""
		}
	}
	return ""
}

// PagesOf reads the tree of pages under root (importProvider.PageSource).
func (p *Provider) PagesOf(ctx context.Context, j *importModels.Job, _ importProvider.JobOptions, root string, read func(pages int)) ([]importProvider.SourcePage, error) {
	tok, err := p.loadToken(ctx, j)
	if err != nil {
		return nil, err
	}
	if j.TriggeredBy == nil {
		return nil, errors.New("job has no triggered_by user")
	}
	t := &tree{p: p, ctx: ctx, tok: tok, job: j, read: read, titles: map[string]string{}, tables: map[string][]string{}}
	if elsewhere, err := t.inAnotherTree(pageKey(root)); err != nil || elsewhere {
		return nil, err
	}
	if err := t.page(pageKey(root), ""); err != nil {
		return nil, err
	}
	out := make([]importProvider.SourcePage, 0, len(t.pages))
	for _, w := range t.pages {
		html, err := t.html(w)
		if err != nil {
			return nil, err
		}
		out = append(out, importProvider.SourcePage{SourceID: w.key, ParentSourceID: w.parent, Title: w.title, Icon: w.icon, HTML: html, Files: w.files})
	}
	return out, nil
}

// inAnotherTree says whether a page planning took for a root, one sitting in
// a block, is in a page the connection reads, whose own tree brings it: a live
// page that isn't a database's row.
func (t *tree) inAnotherTree(key string) (bool, error) {
	var pg notionPage
	if err := t.get("/pages/"+url.PathEscape(key), &pg); err != nil {
		if unreadable(err) {
			return false, nil
		}
		return false, err
	}
	if pg.Parent == nil || pg.Parent.Type != "block_id" {
		return false, nil
	}
	above := t.p.pageOfBlock(t.ctx, t.tok, pg.Parent.BlockID)
	if above == "" {
		return false, nil
	}
	var in notionPage
	if err := t.get("/pages/"+url.PathEscape(above), &in); err != nil {
		if unreadable(err) {
			return false, nil
		}
		return false, err
	}
	row := in.Parent != nil && (in.Parent.Type == "data_source_id" || in.Parent.Type == "database_id")
	return !in.InTrash && !row, nil
}

// walked is a page as its tree was read.
type walked struct {
	key, parent, title, icon string
	blocks                   []notionBlock
	// files are the pictures Notion keeps in the page, read with it, by the
	// address the page names each by; srcs is that address by its block.
	files map[string][]byte
	srcs  map[string]string
}

// tree is one tree of pages being read.
type tree struct {
	p     *Provider
	ctx   context.Context
	tok   string
	job   *importModels.Job
	read  func(pages int)
	pages []walked
	// titles are the tree's pages by key, with their titles.
	titles map[string]string
	// tables are the tables a database became, by the database's id.
	tables map[string][]string
	// capped is set once the tree reaches maxTreePages.
	capped bool
}

func (t *tree) get(path string, out any) error {
	if t.read != nil {
		t.read(len(t.pages))
	}
	return t.p.call(t.ctx, t.tok, pagesVersion, http.MethodGet, path, nil, out)
}

func (t *tree) post(path string, body, out any) error {
	if t.read != nil {
		t.read(len(t.pages))
	}
	return t.p.call(t.ctx, t.tok, pagesVersion, http.MethodPost, path, body, out)
}

// unreadable is an answer about one thing the connection can't read (not
// shared, gone, or of a kind the API won't give), as opposed to the API or
// the token failing the whole tree.
func unreadable(err error) bool {
	var api *apiError
	return errors.As(err, &api) && api.Status >= 400 && api.Status < 500
}

// note tells the person importing about something left in Notion.
func (t *tree) note(sourceID, code, msg string) {
	noteImport(t.ctx, t.job.Id, nil, importModels.EntityDoc, sourceID, importModels.SeverityWarning, code, msg, nil)
}

// page reads a page, then the pages under it, in the order they stand in it.
func (t *tree) page(key, parent string) error {
	if _, seen := t.titles[key]; seen {
		return nil
	}
	if len(t.pages) >= maxTreePages {
		if !t.capped {
			t.capped = true
			t.note(key, "PAGES_LEFT_OUT", fmt.Sprintf("This tree has more than %d pages; the first %d came across.", maxTreePages, maxTreePages))
		}
		return nil
	}
	var pg notionPage
	if err := t.get("/pages/"+url.PathEscape(key), &pg); err != nil {
		if unreadable(err) {
			t.note(key, "PAGE_LEFT_OUT", "A page couldn't be read from Notion, so it and the pages under it didn't come across: share it with the connection, then import again.")
			return nil
		}
		return err
	}
	if pg.InTrash {
		return nil
	}
	title := pageTitle(pg)
	t.titles[key] = title
	blocks, err := t.children(key, 0)
	if err != nil {
		return err
	}
	w := walked{key: key, parent: parent, title: title, icon: emoji(pg.Icon), blocks: blocks, files: map[string][]byte{}, srcs: map[string]string{}}
	t.pictures(&w, blocks)
	t.pages = append(t.pages, w)
	for _, sub := range subpages(blocks) {
		if err := t.page(sub, key); err != nil {
			return err
		}
	}
	return nil
}

// children reads a block's blocks and theirs, up to maxBlockDepth; a page or a
// database under it is read on its own (page, database).
func (t *tree) children(id string, depth int) ([]notionBlock, error) {
	var out []notionBlock
	cursor := ""
	for i := 0; i < maxPages; i++ {
		path := "/blocks/" + url.PathEscape(id) + "/children?page_size=100"
		if cursor != "" {
			path += "&start_cursor=" + url.QueryEscape(cursor)
		}
		var resp struct {
			Results    []notionBlock `json:"results"`
			HasMore    bool          `json:"has_more"`
			NextCursor string        `json:"next_cursor"`
		}
		if err := t.get(path, &resp); err != nil {
			if unreadable(err) {
				t.note(id, "PAGE_PART_LEFT_OUT", "Part of a page couldn't be read from Notion: a synced block whose original the connection can't open reads this way.")
				return out, nil
			}
			return nil, err
		}
		out = append(out, resp.Results...)
		if !resp.HasMore || resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	for i := range out {
		b := &out[i]
		switch {
		case b.InTrash:
		case b.Type == "child_database":
			if err := t.database(b.ID); err != nil {
				return nil, err
			}
		case b.Type == "child_page":
		case b.HasChildren && depth < maxBlockDepth:
			from := b.ID
			if b.SyncedBlock != nil && b.SyncedBlock.SyncedFrom != nil {
				from = b.SyncedBlock.SyncedFrom.BlockID
			}
			kids, err := t.children(from, depth+1)
			if err != nil {
				return nil, err
			}
			b.Children = kids
		}
	}
	return out, nil
}

// subpages are the pages directly under a page, in the order they stand in
// it, columns and toggles included.
func subpages(blocks []notionBlock) []string {
	var out []string
	for _, b := range blocks {
		if b.InTrash {
			continue
		}
		if b.Type == "child_page" {
			out = append(out, pageKey(b.ID))
			continue
		}
		out = append(out, subpages(b.Children)...)
	}
	return out
}

// pageTitle is a page's title: its title property's text.
func pageTitle(pg notionPage) string {
	for _, prop := range pg.Properties {
		if prop.Type == "title" {
			return plainText(prop.Title)
		}
	}
	return ""
}

// emoji is an icon's emoji, when it is one.
func emoji(icon *notionIcon) string {
	if icon != nil && icon.Type == "emoji" {
		return icon.Emoji
	}
	return ""
}

// plainText is rich text as its plain text.
func plainText(rts []notionRichText) string {
	var b strings.Builder
	for _, rt := range rts {
		b.WriteString(rt.PlainText)
	}
	return strings.TrimSpace(b.String())
}

// ─── Databases, as tables ─────────────────────────────────────────

// database makes a database inside a page into a table for each of its data
// sources, once per import (EntityTable in the id map, so a tree read again
// finds its tables), private to the person importing, as their docs are.
func (t *tree) database(id string) error {
	if _, done := t.tables[id]; done {
		return nil
	}
	t.tables[id] = nil
	var db struct {
		Title       []notionRichText `json:"title"`
		Icon        *notionIcon      `json:"icon"`
		InTrash     bool             `json:"in_trash"`
		DataSources []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data_sources"`
	}
	if err := t.get("/databases/"+url.PathEscape(id), &db); err != nil {
		if unreadable(err) {
			t.note(id, "DATABASE_LEFT_OUT", "A database in this page couldn't be read, so its doc doesn't show it; a linked view of a database the connection can't open reads this way.")
			return nil
		}
		return err
	}
	if db.InTrash {
		return nil
	}
	name := plainText(db.Title)
	for _, ds := range db.DataSources {
		title := name
		if len(db.DataSources) > 1 && strings.TrimSpace(ds.Name) != "" {
			title = strings.TrimSpace(name + " · " + ds.Name)
		}
		table, err := t.table(ds.ID, title, emoji(db.Icon))
		if err != nil {
			if unreadable(err) {
				t.note(ds.ID, "DATABASE_LEFT_OUT", fmt.Sprintf("The database %q couldn't be read, so it isn't a table.", title))
				continue
			}
			return err
		}
		t.tables[id] = append(t.tables[id], table)
	}
	return nil
}

// table is the table a data source became in this import, made now if it
// isn't there yet.
func (t *tree) table(dataSource, title, icon string) (string, error) {
	made, err := lookupMapped(t.ctx, t.job.Id, importModels.EntityTable, dataSource)
	if err != nil {
		return "", err
	}
	if made != uuid.Nil {
		return made.String(), nil
	}
	var src struct {
		Properties map[string]notionDBProp `json:"properties"`
	}
	if err := t.get("/data_sources/"+url.PathEscape(dataSource), &src); err != nil {
		return "", err
	}
	rows, more, err := t.rows(dataSource)
	if err != nil {
		return "", err
	}
	table, leftOut, err := tableFromDatabase(t.ctx, dataTableBusiness.Actor{UserID: *t.job.TriggeredBy}, notionAdapter.Database{
		Title: title, Icon: icon, Columns: columns(src.Properties), Rows: rows, Private: true,
	})
	if err != nil {
		return "", err
	}
	if leftOut > 0 {
		t.note(dataSource, "TABLE_ROWS_LEFT_OUT", fmt.Sprintf("%d rows of %q couldn't be added to its table.", leftOut, title))
	}
	if err := recordMapped(t.ctx, t.job.Id, importModels.EntityTable, dataSource, table.Id, nil, nil, true); err != nil {
		t.note(dataSource, "TABLE_NOT_RECORDED", fmt.Sprintf("The table %q came across, but the import couldn't note it, so a roll back leaves it in place.", title))
	}
	if more {
		t.note(dataSource, "TABLE_ROWS_LEFT_OUT", fmt.Sprintf("%q has more than %d rows; the first %d came across.", title, maxTableRows, maxTableRows))
	}
	return table.Id.String(), nil
}

// rows reads a data source's rows as values by column name, and says whether
// there were more than a table takes from it.
func (t *tree) rows(dataSource string) ([]map[string]any, bool, error) {
	var out []map[string]any
	cursor := ""
	for len(out) < maxTableRows {
		body := map[string]any{"page_size": 100}
		if cursor != "" {
			body["start_cursor"] = cursor
		}
		var resp struct {
			Results    []notionPage `json:"results"`
			HasMore    bool         `json:"has_more"`
			NextCursor string       `json:"next_cursor"`
		}
		if err := t.post("/data_sources/"+url.PathEscape(dataSource)+"/query", body, &resp); err != nil {
			return nil, false, err
		}
		for _, r := range resp.Results {
			if len(out) == maxTableRows {
				return out, true, nil
			}
			values := map[string]any{}
			for name, prop := range r.Properties {
				if v := cellOf(prop); v != nil {
					values[name] = v
				}
			}
			out = append(out, values)
		}
		if !resp.HasMore || resp.NextCursor == "" {
			return out, false, nil
		}
		cursor = resp.NextCursor
	}
	return out, true, nil
}

// columns are a data source's properties as a table's columns: its title
// first, then the rest by name.
func columns(props map[string]notionDBProp) []notionAdapter.Column {
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Slice(names, func(a, b int) bool {
		ta, tb := props[names[a]].Type == "title", props[names[b]].Type == "title"
		if ta != tb {
			return ta
		}
		return names[a] < names[b]
	})
	out := make([]notionAdapter.Column, 0, len(names))
	for _, n := range names {
		prop := props[n]
		col := notionAdapter.Column{Name: n, Type: prop.Type}
		for _, o := range optionsOf(prop) {
			col.Options = append(col.Options, o.Name)
		}
		out = append(out, col)
	}
	return out
}

func optionsOf(p notionDBProp) []notionOption {
	switch {
	case p.Select != nil:
		return p.Select.Options
	case p.Status != nil:
		return p.Status.Options
	case p.MultiSelect != nil:
		return p.MultiSelect.Options
	}
	return nil
}

// cellOf is a row's value of one property as a table keeps it: text, a
// number, a yes or no, a day, or names; nil for none.
func cellOf(p notionPageProperty) any {
	text := func(s *string) any {
		if s == nil || strings.TrimSpace(*s) == "" {
			return nil
		}
		return *s
	}
	day := func(s string) any {
		if len(s) < 10 {
			return nil
		}
		return s[:10]
	}
	switch p.Type {
	case "title":
		return plainText(p.Title)
	case "rich_text":
		return plainText(p.RichText)
	case "select":
		if p.Select != nil {
			return p.Select.Name
		}
	case "status":
		if p.Status != nil {
			return p.Status.Name
		}
	case "multi_select":
		var names []string
		for _, o := range p.MultiSelect {
			names = append(names, o.Name)
		}
		return names
	case "number":
		if p.Number != nil {
			return *p.Number
		}
	case "date":
		if p.Date != nil {
			return day(p.Date.Start)
		}
	case "checkbox":
		return p.Checkbox
	case "url":
		return text(p.URL)
	case "email":
		return text(p.Email)
	case "phone_number":
		return text(p.Phone)
	case "people":
		return peopleNames(p)
	case "created_time":
		return day(p.CreatedTime)
	case "last_edited_time":
		return day(p.LastEditedTime)
	case "unique_id":
		if p.UniqueID != nil && p.UniqueID.Number != nil {
			n := strconv.Itoa(*p.UniqueID.Number)
			if p.UniqueID.Prefix != nil && *p.UniqueID.Prefix != "" {
				return *p.UniqueID.Prefix + "-" + n
			}
			return n
		}
	case "formula":
		if f := p.Formula; f != nil {
			switch {
			case f.String != nil:
				return text(f.String)
			case f.Number != nil:
				return *f.Number
			case f.Boolean != nil:
				return *f.Boolean
			case f.Date != nil:
				return day(f.Date.Start)
			}
		}
	}
	return nil
}

// ─── Blocks, as the editor's HTML ─────────────────────────────────

// colourAttr is a block's colour as the editor writes it, or "".
func colourAttr(c string) string {
	if colour := notionAdapter.BlockColour(c); colour != "" {
		return ` data-block-color="` + colour + `"`
	}
	return ""
}

// notionPageLink is a link to a page in Notion: "/<id>" or a notion.so
// address ending in the page's id.
var notionPageLink = regexp.MustCompile(`^(?:https://(?:www\.)?notion\.(?:so|site)/[^?#]*?)?/?(?:[^/?#]*-)?([0-9a-f]{32})(?:[?#].*)?$`)

// pageHref is where a link to a Notion page points in OneCamp: the page's key,
// which the import points at its doc, for a page of this tree; the doc an
// earlier tree of the import made; or "" when it didn't come across.
func (t *tree) pageHref(id string) string {
	key := pageKey(id)
	if _, ok := t.titles[key]; ok {
		return key
	}
	if made, err := lookupMapped(t.ctx, t.job.Id, importModels.EntityDoc, key); err == nil && made != uuid.Nil {
		return "/app/doc/" + made.String()
	}
	return ""
}

// linkHref is where a piece of rich text links to: a page of the import, or
// any other address as it is.
func (t *tree) linkHref(rt notionRichText) string {
	href := ""
	if rt.Text != nil && rt.Text.Link != nil {
		href = rt.Text.Link.URL
	} else if rt.Href != nil {
		href = *rt.Href
	}
	if m := notionPageLink.FindStringSubmatch(strings.ToLower(href)); m != nil {
		if to := t.pageHref(m[1]); to != "" {
			return to
		}
	}
	return href
}

// text writes rich text as inline HTML.
func (t *tree) text(rts []notionRichText) string {
	var b strings.Builder
	for _, rt := range rts {
		if rt.Type == "equation" && rt.Equation != nil {
			fmt.Fprintf(&b, `<span data-type="inline-math" data-latex="%s"></span>`, html.EscapeString(rt.Equation.Expression))
			continue
		}
		s := strings.ReplaceAll(html.EscapeString(rt.PlainText), "\n", "<br>")
		if rt.Type == "mention" {
			if rt.Mention != nil && rt.Mention.Page != nil {
				if href := t.pageHref(rt.Mention.Page.ID); href != "" {
					s = `<a href="` + html.EscapeString(href) + `">` + s + `</a>`
				}
			}
			b.WriteString(s)
			continue
		}
		a := rt.Annotations
		if a.Code {
			s = "<code>" + s + "</code>"
		}
		if a.Bold {
			s = "<strong>" + s + "</strong>"
		}
		if a.Italic {
			s = "<em>" + s + "</em>"
		}
		if a.Strikethrough {
			s = "<s>" + s + "</s>"
		}
		if a.Underline {
			s = "<u>" + s + "</u>"
		}
		if name, background := notionAdapter.DocColour(a.Color); name != "" {
			if background {
				s = `<mark data-color="` + name + `">` + s + `</mark>`
			} else {
				s = `<span data-color="` + name + `">` + s + `</span>`
			}
		}
		if href := t.linkHref(rt); href != "" {
			s = `<a href="` + html.EscapeString(href) + `">` + s + `</a>`
		}
		b.WriteString(s)
	}
	return b.String()
}

// html writes a page's blocks as the editor's HTML (what business/ContentCodec
// reads).
func (t *tree) html(w walked) (string, error) {
	var b strings.Builder
	err := t.write(&b, w, w.blocks)
	return b.String(), err
}

// listOf is the list a block is an item of, by its type.
var listOf = map[string]string{"bulleted_list_item": "<ul>", "numbered_list_item": "<ol>", "to_do": `<ul data-type="taskList">`}

func (t *tree) write(b *strings.Builder, w walked, blocks []notionBlock) error {
	for i := 0; i < len(blocks); {
		blk := blocks[i]
		if blk.InTrash {
			i++
			continue
		}
		if open, ok := listOf[blk.Type]; ok {
			j := i
			for j < len(blocks) && blocks[j].Type == blk.Type {
				j++
			}
			b.WriteString(open)
			for _, it := range blocks[i:j] {
				if err := t.item(b, w, it); err != nil {
					return err
				}
			}
			b.WriteString("</" + open[1:3] + ">")
			i = j
			continue
		}
		if err := t.block(b, w, blk); err != nil {
			return err
		}
		i++
	}
	return nil
}

// item writes one item of a list.
func (t *tree) item(b *strings.Builder, w walked, it notionBlock) error {
	switch {
	case it.ToDo != nil:
		fmt.Fprintf(b, `<li data-type="taskItem" data-checked="%t"%s><p>%s</p>`, it.ToDo.Checked, colourAttr(it.ToDo.Color), t.text(it.ToDo.RichText))
	case it.BulletedListItem != nil:
		b.WriteString("<li" + colourAttr(it.BulletedListItem.Color) + "><p>" + t.text(it.BulletedListItem.RichText) + "</p>")
	case it.NumberedListItem != nil:
		b.WriteString("<li" + colourAttr(it.NumberedListItem.Color) + "><p>" + t.text(it.NumberedListItem.RichText) + "</p>")
	default:
		b.WriteString("<li><p></p>")
	}
	if err := t.write(b, w, it.Children); err != nil {
		return err
	}
	b.WriteString("</li>")
	return nil
}

// block writes one block that is not a list's item.
func (t *tree) block(b *strings.Builder, w walked, blk notionBlock) error {
	children := func() error { return t.write(b, w, blk.Children) }
	switch blk.Type {
	case "paragraph":
		if blk.Paragraph != nil {
			b.WriteString("<p" + colourAttr(blk.Paragraph.Color) + ">" + t.text(blk.Paragraph.RichText) + "</p>")
		}
		return children()
	case "heading_1", "heading_2", "heading_3", "heading_4":
		box := map[string]*notionRichTextBox{"heading_1": blk.Heading1, "heading_2": blk.Heading2, "heading_3": blk.Heading3, "heading_4": blk.Heading4}[blk.Type]
		if box == nil {
			return nil
		}
		tag := "h" + blk.Type[len(blk.Type)-1:]
		heading := "<" + tag + colourAttr(box.Color) + ">" + t.text(box.RichText) + "</" + tag + ">"
		if !box.IsToggleable {
			b.WriteString(heading)
			return children()
		}
		b.WriteString("<details><summary>" + heading + "</summary>")
		if err := children(); err != nil {
			return err
		}
		b.WriteString("</details>")
	case "toggle":
		if blk.Toggle == nil {
			return nil
		}
		b.WriteString("<details" + colourAttr(blk.Toggle.Color) + "><summary>" + t.text(blk.Toggle.RichText) + "</summary>")
		if err := children(); err != nil {
			return err
		}
		b.WriteString("</details>")
	case "quote":
		if blk.Quote == nil {
			return nil
		}
		b.WriteString("<blockquote" + colourAttr(blk.Quote.Color) + "><p>" + t.text(blk.Quote.RichText) + "</p>")
		if err := children(); err != nil {
			return err
		}
		b.WriteString("</blockquote>")
	case "callout":
		if blk.Callout == nil {
			return nil
		}
		icon := emoji(blk.Callout.Icon)
		if icon == "" {
			icon = "💡"
		}
		fmt.Fprintf(b, `<div data-type="callout" data-emoji="%s" data-color="%s"><p>%s</p>`, html.EscapeString(icon), notionAdapter.CalloutColour(blk.Callout.Color), t.text(blk.Callout.RichText))
		if err := children(); err != nil {
			return err
		}
		b.WriteString("</div>")
	case "code":
		if blk.Code == nil {
			return nil
		}
		lang := ""
		if l := strings.ToLower(blk.Code.Language); l != "" && l != "plain text" {
			lang = ` class="language-` + html.EscapeString(strings.ReplaceAll(l, " ", "-")) + `"`
		}
		b.WriteString("<pre><code" + lang + ">" + html.EscapeString(plainText(blk.Code.RichText)) + "</code></pre>")
	case "equation":
		if blk.Equation != nil {
			fmt.Fprintf(b, `<div data-type="block-math" data-latex="%s"></div>`, html.EscapeString(blk.Equation.Expression))
		}
	case "divider":
		b.WriteString("<hr>")
	case "table":
		t.tableBlock(b, blk)
	case "image":
		t.picture(b, w, blk)
	case "bookmark", "embed", "link_preview":
		box := map[string]*notionURLBox{"bookmark": blk.Bookmark, "embed": blk.Embed, "link_preview": blk.LinkPreview}[blk.Type]
		if box != nil && box.URL != "" {
			label := plainText(box.Caption)
			if label == "" {
				label = box.URL
			}
			b.WriteString(`<p><a href="` + html.EscapeString(box.URL) + `">` + html.EscapeString(label) + `</a></p>`)
		}
	case "video", "audio", "file", "pdf":
		t.attachment(b, w, blk)
	case "child_page":
		key := pageKey(blk.ID)
		if title, ok := t.titles[key]; ok {
			if title == "" {
				title = "Untitled"
			}
			b.WriteString(`<p><a href="` + key + `">` + html.EscapeString(title) + `</a></p>`)
		}
	case "child_database":
		for _, table := range t.tables[blk.ID] {
			b.WriteString(`<div data-type="table-embed" data-table-id="` + html.EscapeString(table) + `"></div>`)
		}
	case "link_to_page":
		if blk.LinkToPage != nil && blk.LinkToPage.PageID != "" {
			if href := t.pageHref(blk.LinkToPage.PageID); href != "" {
				title := t.titles[pageKey(blk.LinkToPage.PageID)]
				if title == "" {
					title = "A page from Notion"
				}
				b.WriteString(`<p><a href="` + html.EscapeString(href) + `">` + html.EscapeString(title) + `</a></p>`)
			}
		}
	case "table_of_contents":
		b.WriteString(`<nav data-type="toc"></nav>`)
	case "column_list", "column", "synced_block", "tab":
		return children()
	}
	return nil
}

// tableBlock writes a simple table: its rows, the first row as headers when
// the table has a header row, the first cell of each row when it has a header
// column.
func (t *tree) tableBlock(b *strings.Builder, blk notionBlock) {
	header, sideHeader := false, false
	if blk.Table != nil {
		header, sideHeader = blk.Table.HasColumnHeader, blk.Table.HasRowHeader
	}
	b.WriteString("<table>")
	for i, row := range blk.Children {
		if row.TableRow == nil {
			continue
		}
		b.WriteString("<tr>")
		for j, cell := range row.TableRow.Cells {
			tag := "td"
			if (header && i == 0) || (sideHeader && j == 0) {
				tag = "th"
			}
			b.WriteString("<" + tag + "><p>" + t.text(cell) + "</p></" + tag + ">")
		}
		b.WriteString("</tr>")
	}
	b.WriteString("</table>")
}

// pictures reads the pictures Notion keeps in a page's blocks as soon as the
// page is read, since their addresses expire within the hour, into the files
// the page goes in with.
func (t *tree) pictures(w *walked, blocks []notionBlock) {
	for _, blk := range blocks {
		if blk.InTrash {
			continue
		}
		if box := blk.Image; box != nil && (box.External == nil || box.External.URL == "") && box.File != nil && box.File.URL != "" {
			if data, ext, err := t.fetch(box.File.URL); err != nil {
				t.note(w.key, "PICTURE_LEFT_OUT", fmt.Sprintf("A picture in %q couldn't be brought across from Notion.", w.title))
			} else {
				src := "files/" + pageKey(blk.ID) + ext
				w.files[src], w.srcs[blk.ID] = data, src
			}
		}
		t.pictures(w, blk.Children)
	}
}

// picture writes an image: one Notion keeps as the file read with its page
// (pictures), any other at its address.
func (t *tree) picture(b *strings.Builder, w walked, blk notionBlock) {
	box := blk.Image
	if box == nil {
		return
	}
	src := w.srcs[blk.ID]
	if box.External != nil && box.External.URL != "" {
		src = box.External.URL
	}
	if src == "" {
		return
	}
	caption := html.EscapeString(plainText(box.Caption))
	if caption == "" {
		b.WriteString(`<img src="` + html.EscapeString(src) + `">`)
		return
	}
	b.WriteString(`<figure><img src="` + html.EscapeString(src) + `" alt="` + caption + `"><figcaption>` + caption + `</figcaption></figure>`)
}

// fetch reads a picture Notion keeps, with the extension its type has.
func (t *tree) fetch(address string) ([]byte, string, error) {
	resp, err := fetchPicture(t.ctx, address)
	if err != nil {
		return nil, "", err
	}
	if resp.Status != http.StatusOK || resp.Truncated || !strings.HasPrefix(resp.ContentType, "image/") || strings.Contains(resp.ContentType, "svg") {
		return nil, "", fmt.Errorf("notion picture: status %d, %s", resp.Status, resp.ContentType)
	}
	ext := path.Ext(resp.URL.Path)
	if exts, _ := mime.ExtensionsByType(resp.ContentType); len(exts) > 0 {
		ext = exts[0]
	}
	return resp.Body, ext, nil
}

// attachment writes a video, a sound, a PDF or another file: a link to one on
// the web, or its name for one Notion keeps, which stays there.
func (t *tree) attachment(b *strings.Builder, w walked, blk notionBlock) {
	box := map[string]*notionFileBox{"video": blk.Video, "audio": blk.Audio, "file": blk.File, "pdf": blk.PDF}[blk.Type]
	if box == nil {
		return
	}
	name := plainText(box.Caption)
	if name == "" {
		name = box.Name
	}
	if box.External != nil && box.External.URL != "" {
		if name == "" {
			name = box.External.URL
		}
		b.WriteString(`<p><a href="` + html.EscapeString(box.External.URL) + `">` + html.EscapeString(name) + `</a></p>`)
		return
	}
	if name == "" {
		name = "A file"
	}
	b.WriteString("<p>" + html.EscapeString(name) + "</p>")
	t.note(w.key, "FILE_LEFT_IN_NOTION", fmt.Sprintf("%q in %q stays in Notion: files other than pictures aren't brought across.", name, w.title))
}
