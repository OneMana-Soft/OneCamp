package business

import (
	"context"
	"errors"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// docMaker makes an import's pages into docs: through the tree (each under
// its parent, sharing as its parent does) and the Live Doc Writer's seed, never
// by writing HTML into a doc (I2).
type docMaker struct {
	who    accessBusiness.Subject
	user   *dgraphStruct.DgraphUser
	userID uuid.UUID
	parent string
	report func(done, total int)
}

// createDoc makes an empty doc under parentUUID ("" is the top) through the
// tree's create (A3's CreateDocInTree, which records doc.created): private at
// the top, or as its parent is below it, where it then takes the parent's
// sharing (Access CopyOnCreate), as a page made in the sidebar does. The
// pipeline checked that who may add to the parent.
var createDoc = func(ctx context.Context, user *dgraphStruct.DgraphUser, who accessBusiness.Subject, title, icon, parentUUID string) (string, error) {
	var parent *dgraphStruct.DgraphDoc
	if parentUUID != "" {
		rows, err := docBusiness.DocRows(ctx, []string{parentUUID}, who.UserUID)
		if err != nil {
			return "", err
		}
		if len(rows) == 0 {
			return "", docBusiness.ErrNotFound
		}
		parent = rows[0]
	}
	doc, err := docBusiness.CreateDocInTree(ctx, user, docBusiness.NewDoc{Title: title, Icon: icon, ParentUUID: parentUUID}, parent)
	if err != nil {
		return "", err
	}
	if doc == nil || doc.Uuid == "" {
		return "", errors.New("content io: a doc was made with no id")
	}
	if parent != nil {
		from := accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: parent.Uuid}
		to := accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: doc.Uuid}
		if err := accessBusiness.Rules.CopyOnCreate(ctx, who, from, to); err != nil {
			helpers.LogWarnWithContext(ctx, "content io: doc %s could not take its parent's sharing: %v", doc.Uuid, err)
		}
	}
	return doc.Uuid, nil
}

// discardDoc removes, for good, a doc an import made a moment ago and could
// not fill, with any page of the import still under it (one as empty as it
// is: make never discards a doc that holds imported content). It goes to the
// trash as one delete (A3's TrashSubtree), then out of it (PurgeDoc), as its
// owner: the trash keeps nothing nobody wrote.
var discardDoc = func(ctx context.Context, user *dgraphStruct.DgraphUser, who accessBusiness.Subject, docUUID string) error {
	email := ""
	if user != nil {
		email = user.EmailID
	}
	if _, err := docBusiness.TrashSubtree(ctx, docUUID, who.UserUID, who.UserUUID, email); err != nil {
		return err
	}
	_, err := docBusiness.PurgeDoc(ctx, docUUID, who.UserUID, who.UserUUID, false)
	return err
}

type madePage struct {
	page   Page
	uuid   string
	parent string
	title  string
}

// Why a page is listed as skipped, or kept empty, when its content could not
// be written.
const (
	whyNotMade   = "its doc could not be made"
	whyNotFilled = "the document service could not take its content; try again shortly"
	whyLeftEmpty = "its content could not be written, so its doc is empty: import the file again, or delete the doc"
	whyHolder    = "its own content could not be written; the pages under it were imported"
)

// make makes every page into a doc, and says which docs there are now, what
// it left out, and how many docs have their content.
func (m *docMaker) make(ctx context.Context, pages []Page) (out Made) {
	uuids := map[string]string{}
	var made []madePage
	// Every doc first, parents before children, so links between the pages
	// can be pointed at the docs they became.
	for _, pg := range orderPages(pages) {
		parent := m.parent
		if u, ok := uuids[pg.ParentKey]; ok && pg.ParentKey != "" {
			parent = u
		}
		title := cleanTitle(pg.Title)
		id, err := createDoc(ctx, m.user, m.who, title, pageIcon(pg.Icon), parent)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "content io: a doc for %q could not be made: %v", pg.Key, err)
			out.Skipped = append(out.Skipped, Skipped{What: pg.Key, Why: whyNotMade})
			out.TryAgain = true
			continue
		}
		uuids[pg.Key] = id
		made = append(made, madePage{page: pg, uuid: id, parent: parent, title: title})
	}

	// Then each doc's content, parents first. With the document service down
	// (not running, or not answering) every seed fails alike, so the rest are
	// not tried.
	ok := make([]bool, len(made))
	var down bool
	for i, d := range made {
		if !down {
			body := rewriteLinks(d.page, uuids)
			body, lost := m.uploadImages(ctx, d.uuid, d.page, body)
			out.Skipped = append(out.Skipped, lost...)
			_, err := collab.Live.Seed(ctx, d.uuid, contentcodec.ToHTML(body), collab.Options{By: collab.By{UserUUID: m.who.UserUUID, AgentUUID: m.who.AgentUUID}})
			if err != nil {
				helpers.LogErrorWithContext(ctx, "content io: doc %s could not be seeded: %v", d.uuid, err)
				if down = errors.Is(err, collab.ErrUnavailable); down {
					out.TryAgain = true
				}
			}
			ok[i] = err == nil
		}
		if m.report != nil {
			m.report(i+1, len(made))
		}
	}

	// A doc left empty goes, unless pages imported are under it, which would
	// go with it: the deepest first, so a page's fate is known before its
	// parent's. One that can't be removed stays, and the job says so.
	kept := make([]bool, len(made))
	holds := make([]bool, len(made)) // it has content, or content under it
	why := make([]string, len(made))
	for i := len(made) - 1; i >= 0; i-- {
		if ok[i] {
			kept[i], holds[i] = true, true
			out.Filled++
			continue
		}
		for j := i + 1; j < len(made) && !holds[i]; j++ {
			holds[i] = holds[j] && made[j].parent == made[i].uuid
		}
		if holds[i] {
			kept[i], why[i] = true, whyHolder
			continue
		}
		if err := discardDoc(ctx, m.user, m.who, made[i].uuid); err != nil {
			helpers.LogWarnWithContext(ctx, "content io: empty doc %s stays: %v", made[i].uuid, err)
			kept[i], why[i] = true, whyLeftEmpty
			continue
		}
		why[i] = whyNotFilled
	}

	out.Docs = make(map[string]string, len(made))
	for i, d := range made {
		if why[i] != "" {
			out.Skipped = append(out.Skipped, Skipped{What: d.page.Key, Why: why[i]})
		}
		if kept[i] {
			out.Created = append(out.Created, Created{Kind: accessBusiness.KindDoc, UUID: d.uuid, Title: d.title, ParentUUID: d.parent})
			out.Docs[d.page.Key] = d.uuid
		}
	}
	return out
}

// orderPages puts every page after its parent; a page whose parent is not
// among them goes to the top of the import.
func orderPages(pages []Page) []Page {
	byKey := map[string]int{}
	for i, p := range pages {
		byKey[p.Key] = i
	}
	out := make([]Page, 0, len(pages))
	state := make([]int, len(pages)) // 0 new, 1 visiting, 2 placed
	var place func(i int)
	place = func(i int) {
		if state[i] != 0 {
			return
		}
		state[i] = 1
		if j, ok := byKey[pages[i].ParentKey]; ok && pages[i].ParentKey != "" && state[j] == 0 {
			place(j)
		}
		if j, ok := byKey[pages[i].ParentKey]; ok && state[j] == 1 && j != i {
			// A loop: this page goes to the top.
			pages[i].ParentKey = ""
		}
		state[i] = 2
		out = append(out, pages[i])
	}
	for i := range pages {
		place(i)
	}
	return out
}

var notionID = regexp.MustCompile(`\s+[0-9a-f]{32}$`)

// cleanTitle is a page's title as a doc keeps it: one line, without the
// 32-character id Notion's export adds to every name, at most 200 characters.
func cleanTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	t = notionID.ReplaceAllString(t, "")
	if utf8.RuneCountInString(t) > 200 {
		r := []rune(t)
		t = strings.TrimSpace(string(r[:200]))
	}
	return t
}

// pageIcon is an imported page's icon when it is short enough to be one emoji
// (at most 12 characters in 32 bytes); a longer one is no icon.
func pageIcon(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 32 || utf8.RuneCountInString(s) > 12 {
		return ""
	}
	return s
}

// rewriteLinks points a link to another page of the import at the doc it
// became: "Design doc.md" is /app/doc/<id>.
func rewriteLinks(pg Page, uuids map[string]string) *contentcodec.Node {
	dir := path.Dir(pg.Key)
	pg.Body.Walk(func(n *contentcodec.Node) bool {
		for _, m := range n.Marks {
			if m.Type != "link" {
				continue
			}
			href, _ := m.Attrs["href"].(string)
			if target := pageTarget(dir, href, uuids); target != "" {
				m.Attrs["href"] = "/app/doc/" + target
			}
		}
		return true
	})
	return pg.Body
}

// pageTarget is the doc a relative link from dir names, or "".
func pageTarget(dir, href string, uuids map[string]string) string {
	if href == "" || strings.HasPrefix(href, "/") || strings.HasPrefix(href, "#") || strings.HasPrefix(href, "?") {
		return ""
	}
	if u, err := url.Parse(href); err != nil || u.Scheme != "" || u.Host != "" {
		return ""
	}
	p := href
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if unescaped, err := url.PathUnescape(p); err == nil {
		p = unescaped
	}
	p = path.Clean(path.Join(dir, p))
	if id, ok := uuids[p]; ok {
		return id
	}
	// A link may leave out the extension, or name the page as another
	// format of the same export.
	stem := p
	if hasExt(p, pageExtensions...) {
		stem = strings.TrimSuffix(p, path.Ext(p))
	}
	for _, ext := range pageExtensions {
		if id, ok := uuids[stem+ext]; ok {
			return id
		}
	}
	return ""
}

// pageExtensions are the extensions of files that become pages.
var pageExtensions = []string{".md", ".markdown", ".html", ".htm", ".txt", ".docx"}
