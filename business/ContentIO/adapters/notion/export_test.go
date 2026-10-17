package notion

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
)

type collected struct {
	pages   []contentIOBusiness.Page
	skipped []string
}

func (c *collected) Page(p contentIOBusiness.Page) error { c.pages = append(c.pages, p); return nil }
func (c *collected) Progress(int, int)                   {}
func (c *collected) Skip(what, _ string)                 { c.skipped = append(c.skipped, what) }

func archive(t *testing.T, files map[string]string) *contentIOBusiness.ZipArchive {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	z, err := contentIOBusiness.OpenZip(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

// exported is a page as Notion's HTML export writes one: the header with its
// icon and title, then its blocks.
func exported(icon, title, body string) string {
	head := ""
	if icon != "" {
		head = `<div class="page-header-icon undefined"><span class="icon">` + icon + `</span></div>`
	}
	return `<html><head><meta charset="utf-8"/><title>` + title + `</title></head><body><article id="1f2e" class="page sans"><header>` + head +
		`<h1 class="page-title">` + title + `</h1><p class="page-description"></p></header><div class="page-body">` + body + `</div></article></body></html>`
}

const (
	root  = "Engineering 11111111111111111111111111111111"
	child = "Runbooks 22222222222222222222222222222222"
)

// ai-search-io.md P1-8: Notion's HTML export imports with its tree, titles and
// icons, and its blocks become the doc's own.
func TestNotionsHTMLExportKeepsItsTreeTitlesIconsAndBlocks(t *testing.T) {
	z := archive(t, map[string]string{
		root + ".html": exported("🛠️", "Engineering",
			`<p id="a" class="block-color-blue">Plans for <mark class="highlight-red">Q4</mark> and <mark class="highlight-yellow_background">launch</mark>.</p>`+
				`<ul id="b" class="bulleted-list"><li style="list-style-type:disc">One</li></ul><ul id="c" class="bulleted-list"><li style="list-style-type:disc">Two</li></ul>`+
				`<ul id="d" class="to-do-list"><li><div class="checkbox checkbox-on"></div> <span class="to-do-children-checked">Tag it</span></li></ul>`+
				`<ul id="e" class="to-do-list"><li><div class="checkbox checkbox-off"></div> <span class="to-do-children-unchecked">Announce</span></li></ul>`+
				`<ul id="f" class="toggle"><li><details open=""><summary>Rollback</summary><p>Revert the tag.</p></details></li></ul>`+
				`<figure id="g" class="block-color-gray_background callout" style="white-space:pre-wrap;display:flex"><div style="font-size:1.5em"><span class="icon">⚠️</span></div><div style="width:100%">Never on Fridays.</div></figure>`+
				`<figure id="h" class="equation"><div class="equation-container"><span class="katex-display"><math><semantics><annotation encoding="application/x-tex">e=mc^2</annotation></semantics></math></span></div></figure>`+
				`<p id="i">Energy <span class="notion-text-equation-token"><math><semantics><annotation encoding="application/x-tex">E</annotation></semantics></math></span> moves.</p>`+
				`<div id="j" class="column-list"><div class="column"><p>Left</p></div><div class="column"><p>Right</p></div></div>`+
				`<figure id="k" class="link-to-page"><a href="Engineering%2011111111111111111111111111111111/Runbooks%2022222222222222222222222222222222.html"><span class="icon">📄</span>Runbooks</a></figure>`+
				`<figure id="l" class="image"><a href="Engineering%2011111111111111111111111111111111/diagram.png"><img src="Engineering%2011111111111111111111111111111111/diagram.png"/></a><figcaption>The flow</figcaption></figure>`),
		root + "/" + child + ".html":                    exported("", "Runbooks", `<p>Restart the queue.</p>`),
		root + "/diagram.png":                           "png",
		"Releases 33333333333333333333333333333333.csv": "Name,Stage\nv2.73,Shipped\n",
	})
	if !(export{}).Claims(z.Names()) {
		t.Fatal("the export was not claimed")
	}
	var c collected
	if err := (export{}).Read(context.Background(), z, &c); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]contentIOBusiness.Page{}
	for _, p := range c.pages {
		byKey[p.Key] = p
	}
	eng, rb := byKey[root+".html"], byKey[root+"/"+child+".html"]
	if len(c.pages) != 2 || eng.Title != "Engineering" || eng.Icon != "🛠️" || rb.Title != "Runbooks" || rb.ParentKey != eng.Key {
		t.Fatalf("pages %+v", c.pages)
	}
	if len(c.skipped) != 1 || !strings.HasSuffix(c.skipped[0], ".csv") {
		t.Fatalf("skipped %v", c.skipped)
	}

	md := contentcodec.ToMarkdown(eng.Body, contentcodec.MarkdownOptions{})
	for _, want := range []string{"- One\n- Two", "* [x] Tag it\n* [ ] Announce", "<summary>Rollback</summary>", "Never on Fridays.", "```math\ne=mc^2\n```",
		"$`E`$", "Left\n\nRight", "[Runbooks](Engineering%2011111111111111111111111111111111/Runbooks%2022222222222222222222222222222222.html)"} {
		if !strings.Contains(md, want) {
			t.Errorf("missing %q in:\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"🛠️", "Engineering\n", "📄", "⚠️\n"} {
		if strings.HasPrefix(md, unwanted) || strings.Contains(md, "\n"+unwanted) {
			t.Errorf("%q, from the page's header or an icon, is in the body:\n%s", unwanted, md)
		}
	}
	kinds := map[string]map[string]any{}
	eng.Body.Walk(func(n *contentcodec.Node) bool {
		if _, seen := kinds[n.Type]; !seen {
			kinds[n.Type] = n.Attrs
		}
		for _, m := range n.Marks {
			kinds["mark:"+m.Type] = m.Attrs
		}
		return true
	})
	if a := kinds["callout"]; a == nil || a["emoji"] != "⚠️" || a["color"] != "gray" {
		t.Errorf("callout %v", a)
	}
	if a := kinds["paragraph"]; a == nil || a["blockColor"] != "sky" {
		t.Errorf("the first paragraph's colour: %v", a)
	}
	if a := kinds["mark:textStyle"]; a == nil || a["color"] != "berry" {
		t.Errorf("a red word: %v", a)
	}
	if a := kinds["mark:highlight"]; a == nil || a["color"] != "sun" {
		t.Errorf("a yellow highlight: %v", a)
	}
	if a := kinds["image"]; a == nil || a["caption"] != "The flow" {
		t.Errorf("image %v", a)
	}
}

// Notion's Markdown export reads as the zip of Markdown it is, titled without
// the ids its names carry.
func TestNotionsMarkdownExportImportsByItsFolders(t *testing.T) {
	z := archive(t, map[string]string{
		root + ".md":               "# Engineering\n\nSee [Runbooks](Engineering%2011111111111111111111111111111111/Runbooks%2022222222222222222222222222222222.md).",
		root + "/" + child + ".md": "# Runbooks\n\nRestart the queue.",
		"Releases 3333.csv":        "not an export name",
		"Releases 33333333333333333333333333333333.csv": "Name\nv2.73\n",
	})
	var c collected
	if err := (export{}).Read(context.Background(), z, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.pages) != 2 || c.pages[0].Title != "Engineering" || c.pages[1].Title != "Runbooks" || c.pages[1].ParentKey != c.pages[0].Key {
		t.Fatalf("pages %+v", c.pages)
	}
	if len(c.skipped) != 2 {
		t.Fatalf("skipped %v", c.skipped)
	}
}

func TestAZipWithoutNotionNamesIsNotClaimed(t *testing.T) {
	if (export{}).Claims([]string{"Plans.md", "Plans/Q4.md", "notes 1234.html"}) {
		t.Fatal("a plain zip was claimed")
	}
}

// Notion nests a toggle in a toggle, a to-do under a to-do and a callout in a
// callout; each inner one becomes the doc's own block too, not a list holding
// Notion's markup.
func TestNestedTogglesToDosAndCalloutsBecomeTheDocsOwn(t *testing.T) {
	z := archive(t, map[string]string{
		root + ".html": exported("", "Engineering",
			`<ul class="toggle"><li><details open=""><summary>Outer</summary><ul class="toggle"><li><details open=""><summary>Inner</summary><p>Body</p></details></li></ul></details></li></ul>`+
				`<ul class="to-do-list"><li><div class="checkbox checkbox-off"></div> <span class="to-do-children-unchecked">Parent</span><div class="indented">`+
				`<ul class="to-do-list"><li><div class="checkbox checkbox-on"></div> <span class="to-do-children-checked">Child</span></li></ul></div></li></ul>`+
				`<figure class="callout"><div style="font-size:1.5em"><span class="icon">💡</span></div><div style="width:100%">Outer note`+
				`<figure class="callout"><div style="font-size:1.5em"><span class="icon">⚠️</span></div><div style="width:100%">Inner note</div></figure></div></figure>`),
	})
	var c collected
	if err := (export{}).Read(context.Background(), z, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.pages) != 1 {
		t.Fatalf("pages %+v", c.pages)
	}
	count := map[string]int{}
	var emojis, checked []string
	c.pages[0].Body.Walk(func(n *contentcodec.Node) bool {
		count[n.Type]++
		switch n.Type {
		case "callout":
			emojis = append(emojis, n.Attrs["emoji"].(string))
		case "taskItem":
			checked = append(checked, fmt.Sprint(n.Attrs["checked"]))
		}
		return true
	})
	if count["collapsible"] != 2 || count["taskList"] != 2 || count["callout"] != 2 || count["bulletList"] != 0 {
		t.Errorf("blocks %v", count)
	}
	if strings.Join(emojis, " ") != "💡 ⚠️" || strings.Join(checked, " ") != "false true" {
		t.Errorf("callouts %v, to-dos checked %v", emojis, checked)
	}
}

// A list nested forty deep is joined in one walk: each level's lists are
// read once, not once for every list above them.
func TestADeeplyNestedListIsJoinedInOneWalk(t *testing.T) {
	const depth = 40
	var b strings.Builder
	for i := 0; i < depth; i++ {
		fmt.Fprintf(&b, `<ul class="bulleted-list"><li style="list-style-type:disc">Level %d`, i)
	}
	for i := 0; i < depth; i++ {
		b.WriteString(`</li></ul>`)
	}
	z := archive(t, map[string]string{root + ".html": exported("", "Engineering", b.String()+`<ul class="bulleted-list"><li>After</li></ul>`)})
	done := make(chan error, 1)
	var c collected
	go func() { done <- (export{}).Read(context.Background(), z, &c) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("joining a list nested forty deep took more than ten seconds")
	}
	md := contentcodec.ToMarkdown(c.pages[0].Body, contentcodec.MarkdownOptions{})
	if !strings.Contains(md, "- Level 0") || !strings.Contains(md, "Level 3") || !strings.Contains(md, "\n- After") {
		t.Errorf("the list came across as:\n%s", md)
	}
}
