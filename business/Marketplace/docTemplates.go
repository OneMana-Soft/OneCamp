package business

// Doc templates (docs programme B5): a body to start a doc from, as a kind of
// template beside agents, automations and tables. The kind is free text in
// marketplace_templates (migration 93), so it needs no migration.
//
// OneCamp's own templates (docTemplatesBuiltin.go) are listed first, under
// fixed ids, and never stored: every workspace has them, and none can remove
// them. A team's own are stored like any template, made from a doc ("Save as
// template", business/DocHome) or published with a body.
//
// A template's body is written by the server as a doc is made from it, through
// the live doc's seed (business/Doc CreateDocInTree reads it from Templates,
// set below), never by the browser after the doc has opened: two tabs opening
// a new doc together each laid it in (N37). Lists carry a preview, never the
// body: a gallery of long templates was a long download.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"time"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/Marketplace"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// KindDoc is a doc template.
const KindDoc = "doc"

// validKind is a kind of template this package publishes and lists: the
// model's kinds, and doc.
func validKind(k string) bool { return model.ValidKind(k) || k == KindDoc }

// DocPayload is a doc template's payload. A list carries Preview and no Body;
// one template read on its own carries Body.
type DocPayload struct {
	// Title is the title a doc made from it starts with.
	Title string `json:"title"`
	// Body is the editor's HTML, with no block ids (each doc gets its own).
	Body    string `json:"body,omitempty"`
	Preview string `json:"preview,omitempty"`
	// Builtin is one of OneCamp's own templates.
	Builtin bool `json:"builtin"`
	// Hue and Glyph draw its tile (the camp hues; a glyph's name).
	Hue      string `json:"hue,omitempty"`
	Glyph    string `json:"glyph,omitempty"`
	Category string `json:"category,omitempty"`
}

const (
	// maxDocTitleLen bounds a template's title, as a doc's title is bounded.
	maxDocTitleLen = 200
	// docPreviewBytes is how much of a body a list shows: a card's first lines.
	docPreviewBytes = 1500
	// defaultGlyph and teamCategory are what a team's own template shows.
	defaultGlyph = "file-text"
	teamCategory = "Workspace"
)

// HueFor is a stable hue for an id, for a template that names none.
func HueFor(id string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(id)))
	return helpers.CampHues[int(h.Sum32()%uint32(len(helpers.CampHues)))]
}

var (
	errBuiltinTemplate = errors.New("OneCamp's own templates can't be removed.")
	// ErrEmptyTemplate is a body with nothing in it.
	ErrEmptyTemplate = errors.New("There's nothing in this doc to make a template of yet.")
	// ErrTemplateTooLarge is a body over the payload's cap.
	ErrTemplateTooLarge = fmt.Errorf("This doc is too long to be a template. A template holds up to %d KB.", maxPayloadBytes/1024)
	// ErrTemplateName is a template with no name.
	ErrTemplateName = errors.New("Give the template a name.")
)

var blockIDAttr = regexp.MustCompile(`\s+data-block-id="[^"]*"`)

// StripBlockIDs takes the block ids out of a body: blocks are given ids of
// their own in each doc (A1), and a template's are not anyone's. Pure.
func StripBlockIDs(html string) string {
	return blockIDAttr.ReplaceAllString(html, "")
}

// blockEnds are the closing tags a preview may end on.
var blockEnds = []string{"</p>", "</h1>", "</h2>", "</h3>", "</h4>", "</li>", "</ul>", "</ol>", "</blockquote>", "</pre>", "</div>"}

// DocPreview is a body's opening, for a card: at most docPreviewBytes, ending
// where a block ends, so no tag or character is cut in half (the browser
// closes what is left open). Pure.
func DocPreview(body string) string {
	body = strings.TrimSpace(StripBlockIDs(body))
	if len(body) <= docPreviewBytes {
		return body
	}
	cut := body[:docPreviewBytes]
	best := -1
	for _, end := range blockEnds {
		if i := strings.LastIndex(cut, end); i >= 0 && i+len(end) > best {
			best = i + len(end)
		}
	}
	if best > 0 {
		return cut[:best]
	}
	if i := strings.LastIndex(cut, "<"); i >= 0 {
		cut = cut[:i]
	}
	return strings.ToValidUTF8(cut, "")
}

// BlankBody is whether a body holds nothing anyone wrote: no text, no
// picture, no block of the editor's own (a to-do, a callout).
func BlankBody(body string) bool {
	return helpers.HTMLToPlainText(body) == "" && !strings.Contains(body, "<img") && !strings.Contains(body, "data-type=")
}

// normalDocPayload checks a doc template's payload and gives it its stored
// form: a body with something in it and no block ids, a title, a tile.
func normalDocPayload(p DocPayload, id string) (DocPayload, error) {
	p.Title = strings.TrimSpace(p.Title)
	if len([]rune(p.Title)) > maxDocTitleLen {
		return p, fmt.Errorf("The template's title is longer than %d characters.", maxDocTitleLen)
	}
	p.Body = strings.TrimSpace(StripBlockIDs(p.Body))
	if BlankBody(p.Body) {
		return p, ErrEmptyTemplate
	}
	if len(p.Body) > maxPayloadBytes {
		return p, ErrTemplateTooLarge
	}
	p.Preview = ""
	p.Builtin = false
	if !helpers.IsCampHue(p.Hue) {
		p.Hue = HueFor(id)
	}
	if strings.TrimSpace(p.Glyph) == "" || len(p.Glyph) > maxIconLen {
		p.Glyph = defaultGlyph
	}
	if p.Category = strings.TrimSpace(p.Category); p.Category == "" || len(p.Category) > maxIconLen {
		p.Category = teamCategory
	}
	return p, nil
}

// readDocPayload reads a stored doc template's payload.
func readDocPayload(raw string) (DocPayload, error) {
	var p DocPayload
	err := json.Unmarshal([]byte(raw), &p)
	return p, err
}

// listedPayload is a doc template's payload as a list carries it: its
// preview in place of its body.
func listedPayload(p DocPayload) string {
	p.Preview = DocPreview(p.Body)
	p.Body = ""
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// fullPayload is a doc template's payload as one template's read carries it.
func fullPayload(p DocPayload) string {
	p.Preview = ""
	p.Body = StripBlockIDs(p.Body)
	b, err := json.Marshal(p)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// asListed gives a doc template, as stored, its list form. Other kinds are
// left as they are.
func asListed(t *model.Template) *model.Template {
	if t == nil || t.Kind != KindDoc {
		return t
	}
	p, err := readDocPayload(t.Payload)
	if err != nil {
		t.Payload = "{}"
		return t
	}
	t.Payload = listedPayload(p)
	return t
}

// builtinEpoch dates OneCamp's own templates.
var builtinEpoch = time.Date(2026, time.October, 13, 0, 0, 0, 0, time.UTC)

// builtinTemplate is one of OneCamp's own templates, as the marketplace
// answers it: listed (a preview) or full (the body).
func builtinTemplate(b BuiltinDocTemplate, full bool) *model.Template {
	p := DocPayload{Title: b.Name, Body: b.Body, Builtin: true, Hue: b.Hue, Glyph: b.Glyph, Category: b.Category}
	payload := listedPayload(p)
	if full {
		payload = fullPayload(p)
	}
	desc := b.Description
	return &model.Template{
		Id:          b.ID,
		Kind:        KindDoc,
		Name:        b.Name,
		Description: &desc,
		Payload:     payload,
		CreatedBy:   uuid.Nil,
		CreatedAt:   builtinEpoch,
		UpdatedAt:   builtinEpoch,
		AuthorName:  "OneCamp",
	}
}

// builtinsListed is OneCamp's own doc templates, in their order, listed.
func builtinsListed() []*model.Template {
	out := make([]*model.Template, 0, len(BuiltinDocTemplates))
	for _, b := range BuiltinDocTemplates {
		out = append(out, builtinTemplate(b, false))
	}
	return out
}

// builtinByID is one of OneCamp's own templates by id.
func builtinByID(id uuid.UUID) (BuiltinDocTemplate, bool) {
	for _, b := range BuiltinDocTemplates {
		if b.ID == id {
			return b, true
		}
	}
	return BuiltinDocTemplate{}, false
}

// DocTemplate is a doc template by id, OneCamp's or the workspace's, with its
// body. docBusiness.ErrNoTemplate for an id that names none.
func DocTemplate(ctx context.Context, id string) (DocPayload, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return DocPayload{}, docBusiness.ErrNoTemplate
	}
	if b, ok := builtinByID(parsed); ok {
		return DocPayload{Title: b.Name, Body: StripBlockIDs(b.Body), Builtin: true, Hue: b.Hue, Glyph: b.Glyph, Category: b.Category}, nil
	}
	t, err := model.GetTemplate(ctx, parsed)
	if err != nil {
		return DocPayload{}, err
	}
	if t == nil || t.Kind != KindDoc {
		return DocPayload{}, docBusiness.ErrNoTemplate
	}
	p, err := readDocPayload(t.Payload)
	if err != nil || BlankBody(p.Body) {
		return DocPayload{}, docBusiness.ErrNoTemplate
	}
	p.Body = StripBlockIDs(p.Body)
	return p, nil
}

// docTemplates is where a new doc's template body comes from (business/Doc
// Templates): this package's doc templates.
type docTemplates struct{}

func (docTemplates) Body(ctx context.Context, templateID string) (string, error) {
	p, err := DocTemplate(ctx, templateID)
	if err != nil {
		return "", err
	}
	return p.Body, nil
}

func init() {
	docBusiness.Templates = docTemplates{}
}

// installDoc makes a doc from a doc template for the person, at the top of
// their private pages, with the template's title; the server writes its body.
func installDoc(ctx context.Context, id uuid.UUID, userInfo *userModels.UserInfo) (*InstallResult, error) {
	p, err := DocTemplate(ctx, id.String())
	if err != nil {
		return nil, err
	}
	private := true
	doc, err := docBusiness.CreateDocInTree(ctx, &userInfo.UserDgraphInfo, docBusiness.NewDoc{
		Title: p.Title, Private: &private, Space: "private", TemplateID: id.String(),
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to make a doc from the template: %w", err)
	}
	return &InstallResult{Kind: KindDoc, EntityID: doc.Uuid, Name: p.Title}, nil
}

// PublishDocTemplate stores a doc template the person made (from a doc: Save
// as template) and answers it as a list carries it.
func PublishDocTemplate(ctx context.Context, createdBy uuid.UUID, name, description, icon string, p DocPayload) (*model.Template, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, ErrTemplateName
	}
	if len([]rune(name)) > maxNameLen {
		return nil, fmt.Errorf("A template's name can be up to %d characters.", maxNameLen)
	}
	if len(description) > maxDescriptionLen {
		return nil, fmt.Errorf("A template's description can be up to %d characters.", maxDescriptionLen)
	}
	if len(icon) > maxIconLen {
		icon = ""
	}
	id := uuid.New()
	if p.Title == "" {
		p.Title = name
	}
	p, err := normalDocPayload(p, id.String())
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPayloadBytes {
		return nil, ErrTemplateTooLarge
	}
	stored, err := model.CreateTemplate(ctx, KindDoc, name, optStr(description), optStr(icon), string(raw), createdBy)
	if err != nil {
		return nil, fmt.Errorf("failed to publish template")
	}
	t, err := model.GetTemplate(ctx, stored)
	if err != nil || t == nil {
		return nil, fmt.Errorf("failed to publish template")
	}
	return asListed(t), nil
}
