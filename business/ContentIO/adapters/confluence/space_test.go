package confluence

import (
	"archive/zip"
	"bytes"
	"context"
	"strings"
	"testing"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
)

type collected struct{ pages []contentIOBusiness.Page }

func (c *collected) Page(p contentIOBusiness.Page) error { c.pages = append(c.pages, p); return nil }
func (c *collected) Progress(int, int)                   {}
func (c *collected) Skip(string, string)                 {}

// exported is a page as Confluence's HTML export writes one: breadcrumbs, a
// "space : page" title, #main-content, and the attachments section outside
// it.
func exported(title, crumbs, content string) string {
	return `<!DOCTYPE html><html><head><title>Engineering : ` + title + `</title></head><body><div id="page"><div id="main" class="aui-page-panel">` +
		`<div id="main-header"><div id="breadcrumb-section"><ol id="breadcrumbs"><li class="first"><span><a href="index.html">Engineering</a></span></li>` + crumbs + `</ol></div>` +
		`<h1 id="title-heading" class="pagetitle"><span id="title-text"> Engineering : ` + title + ` </span></h1></div>` +
		`<div id="content" class="view"><div class="page-metadata">Created by Maya Chen</div>` +
		`<div id="main-content" class="wiki-content group">` + content + `</div>` +
		`<div class="pageSection group"><div class="pageSectionHeader"><h2 id="attachments" class="pageSectionTitle">Attachments:</h2></div><div class="greybox"><a href="attachments/2/9.png">diagram.png</a></div></div>` +
		`</div></div></div></body></html>`
}

func archive(t *testing.T, files map[string]string) *contentIOBusiness.ZipArchive {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(data))
	}
	_ = zw.Close()
	z, err := contentIOBusiness.OpenZip(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

// ai-search-io.md P2-6: a space's export imports with its page titles and its
// tree; its macros become the doc's blocks.
func TestASpaceExportKeepsItsTitlesAndTree(t *testing.T) {
	z := archive(t, map[string]string{
		"ENG/index.html":              `<html><title>Engineering</title><body>Available Pages:</body></html>`,
		"ENG/styles/site.css":         "body{}",
		"ENG/Engineering-Home_1.html": exported("Engineering Home", "", `<p>Welcome.</p>`),
		"ENG/Runbooks_2.html":         exported("Runbooks", `<li><span><a href="Engineering-Home_1.html">Engineering Home</a></span></li>`, `<p>See <a href="Deploys_3.html">Deploys</a>.</p><p><img class="confluence-embedded-image" src="attachments/2/9.png"></p>`),
		"ENG/Deploys_3.html": exported("Deploys", `<li><span><a href="Engineering-Home_1.html">Engineering Home</a></span></li><li><span><a href="Runbooks_2.html">Runbooks</a></span></li>`,
			`<div class="confluence-information-macro confluence-information-macro-warning"><span class="aui-icon confluence-information-macro-icon"></span><div class="confluence-information-macro-body"><p>Never on Fridays.</p></div></div>`+
				`<div class="code panel pdl"><div class="codeContent panelContent pdl"><pre class="syntaxhighlighter-pre" data-syntaxhighlighter-params="brush: bash; gutter: false">make deploy
make verify</pre></div></div>`+
				`<div class="expand-container"><div class="expand-control"><span class="expand-control-text">Rollback steps</span></div><div class="expand-content"><p>Revert the tag.</p></div></div>`+
				`<ul class="inline-task-list"><li class="checked"><span>Tag</span></li><li><span>Announce</span></li></ul>`),
		"ENG/attachments/2/9.png": "png",
	})
	if !(space{}).Claims(z.Names()) {
		t.Fatal("the space export was not claimed")
	}
	var c collected
	if err := (space{}).Read(context.Background(), z, &c); err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]contentIOBusiness.Page{}
	for _, p := range c.pages {
		byTitle[p.Title] = p
	}
	home, runbooks, deploys := byTitle["Engineering Home"], byTitle["Runbooks"], byTitle["Deploys"]
	if len(c.pages) != 3 || home.ParentKey != "" || runbooks.ParentKey != home.Key || deploys.ParentKey != runbooks.Key {
		t.Fatalf("pages %+v", c.pages)
	}
	md := contentcodec.ToMarkdown(deploys.Body, contentcodec.MarkdownOptions{})
	for _, want := range []string{"> [!", "Never on Fridays.", "```bash\nmake deploy\nmake verify\n```", "<details>", "Rollback steps", "- [x] Tag", "- [ ] Announce"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	if strings.Contains(md, "Created by") || strings.Contains(md, "Attachments:") {
		t.Errorf("text from outside the page's content:\n%s", md)
	}
	runbooksMD := contentcodec.ToMarkdown(runbooks.Body, contentcodec.MarkdownOptions{})
	if !strings.Contains(runbooksMD, "(Deploys_3.html)") || !strings.Contains(runbooksMD, "attachments/2/9.png") {
		t.Errorf("links and images should stay relative for the import to point them: %s", runbooksMD)
	}
}

func TestAZipWithoutASpaceIsNotClaimed(t *testing.T) {
	if (space{}).Claims([]string{"site/index.html", "site/about.html"}) {
		t.Fatal("a plain site was claimed")
	}
}

// An expand inside an expand, and a task list under a task, become the doc's
// own blocks at every level.
func TestNestedExpandsAndTaskListsBecomeTheDocsOwn(t *testing.T) {
	z := archive(t, map[string]string{
		"ENG/index.html":      `<html><title>Engineering</title></html>`,
		"ENG/styles/site.css": "body{}",
		"ENG/Deploys_3.html": exported("Deploys", "",
			`<div class="expand-container"><div class="expand-control"><span class="expand-control-text">Outer</span></div><div class="expand-content"><p>A</p>`+
				`<div class="expand-container"><div class="expand-control"><span class="expand-control-text">Inner</span></div><div class="expand-content"><p>B</p></div></div></div></div>`+
				`<ul class="inline-task-list"><li class="checked"><span>Parent</span><ul class="inline-task-list"><li><span>Child</span></li></ul></li></ul>`),
	})
	var c collected
	if err := (space{}).Read(context.Background(), z, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.pages) != 1 {
		t.Fatalf("pages %+v", c.pages)
	}
	count := map[string]int{}
	c.pages[0].Body.Walk(func(n *contentcodec.Node) bool {
		count[n.Type]++
		return true
	})
	if count["collapsible"] != 2 || count["taskList"] != 2 || count["taskItem"] != 2 || count["bulletList"] != 0 {
		t.Errorf("blocks %v", count)
	}
}
