package business

import (
	"strings"
	"testing"
)

const (
	docID   = "0b7c1d2e-3f40-4a51-8b62-7c83d94ea5f6"
	otherID = "9e8d7c6b-5a49-4382-9170-6f5e4d3c2b1a"
	fileA   = "11111111-2222-4333-8444-555555555555"
	fileB   = "66666666-7777-4888-9999-aaaaaaaaaaaa"
)

func pageOf(saved string) string {
	return renderer{docUUID: docID, fileURL: fileURL(Ref{Slug: "launch-notes"}, "")}.body(saved)
}

// testAddresses puts the server and the web app at example addresses.
func testAddresses(t *testing.T) {
	t.Helper()
	oldW, oldA := webBase, apiBase
	t.Cleanup(func() { webBase, apiBase = oldW, oldA })
	webBase = func() string { return "https://app.example.test" }
	apiBase = func() string { return "https://api.example.test" }
}

func att(doc, file string) string {
	return "https://api.example.test/doc/getDocAttachment/" + doc + "/" + file
}

// What runs, styles, posts or frames never reaches the page, whatever the
// saved HTML holds.
func TestNothingThatRunsReachesThePage(t *testing.T) {
	saved := `<p onclick="steal()">Hi <a href="javascript:alert(1)">there</a></p>
<script>alert(1)</script><style>body{display:none}</style>
<img src="x" onerror="alert(1)"><img src="data:image/png;base64,AAAA">
<iframe srcdoc="<script>alert(1)</script>"></iframe><svg><script>alert(1)</script></svg>
<form action="https://evil.example"><input name="p"><button>Go</button></form>
<a href="https://ok.example" target="_blank" onmouseover="x()">ok</a>
<p style="background:url(https://track.example/p.gif)">styled</p>
<object data="x.swf"></object><embed src="x"><base href="https://evil.example/">
<meta http-equiv="refresh" content="0;url=https://evil.example"><link rel="stylesheet" href="https://evil.example/x.css">
<math><mi>x</mi></math><template><img src=x onerror=alert(1)></template>`
	out := strings.ToLower(pageOf(saved))
	for _, bad := range []string{"<script", "javascript:", "onerror", "onclick", "onmouseover", "style=", "srcdoc", "data:",
		"<iframe", "<svg", "<form", "<input", "<button", "<object", "<embed", "<base", "<meta", "<link", "<style", "<math",
		"<template", "target=", "evil.example", "track.example"} {
		if strings.Contains(out, bad) {
			t.Errorf("the page keeps %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `<a href="https://ok.example" rel="nofollow noreferrer">ok</a>`) {
		t.Errorf("a link to the web keeps its address, and nothing tells where it came from:\n%s", out)
	}
	if !strings.Contains(out, "there") || !strings.Contains(out, "styled") {
		t.Errorf("words stay:\n%s", out)
	}
}

// A block showing live workspace data is a note; a mention is its words; a
// link into the app loses its address.
func TestWorkspaceDataStaysInTheWorkspace(t *testing.T) {
	testAddresses(t)
	saved := `<p>Ask <span data-type="mention" data-id="uid-1" data-label="Priya">@Priya</span> about
<span data-type="entityMention" data-id="task-9" data-kind="task">Ship the beta</span> in
<span data-type="channelMention" data-id="c-1">#launch</span>.</p>
<div data-type="task-list-embed" data-project="p-1"></div>
<div data-type="table-embed" data-table="t-1"></div><div data-type="board-embed" data-board="b-1"></div>
<div data-type="recording-embed" data-recording="r-1"></div><div data-type="chart" data-source="t-1"></div>
<div data-type="poll" data-poll="poll-1"></div><div data-type="button" data-action="a-1"></div>
<div data-type="future-thing-embed" data-x="1"></div><div data-type="toc"></div>
<p><a href="/app/doc/` + otherID + `">the plan</a> and <a href="https://app.example.test/app/channel/c-1">the channel</a></p>
<div data-type="subpage" data-id="` + otherID + `">Budget</div>
<p><span data-type="link-mention" data-href="https://example.org/spec">The spec</span></p>
<div data-type="bookmark" data-url="https://APP.example.test/app/doc/x-1" data-title="Our page"></div>
<div data-type="web-embed" data-src="https://api.example.test/doc/getDocInfo/x-2"></div>`
	out := pageOf(saved)
	for _, gone := range []string{"uid-1", "task-9", "c-1", "p-1", "t-1", "b-1", "r-1", "poll-1", "a-1", otherID, "/app/", "data-",
		"x-1", "x-2", "app.example.test", "api.example.test"} {
		if strings.Contains(out, gone) {
			t.Errorf("the page keeps %q:\n%s", gone, out)
		}
	}
	if n := strings.Count(out, `class="oc-hidden"`); n != 10 {
		t.Errorf("%d notes, want 10 (seven live blocks, a block named as an embed, the contents, an embed of the app):\n%s", n, out)
	}
	for _, words := range []string{"@Priya", "Ship the beta", "#launch", "the plan", "the channel", "Budget", "Our page"} {
		if !strings.Contains(out, words) {
			t.Errorf("the page lost the words %q:\n%s", words, out)
		}
	}
	if !strings.Contains(out, `<a href="https://example.org/spec" rel="nofollow noreferrer">The spec</a>`) {
		t.Errorf("a mention of a web page links to it:\n%s", out)
	}
}

// A picture or a file is shown only when it is this doc's own, by the page's
// own address; another doc's never is (D-25, condition 3).
func TestOnlyTheDocsOwnFilesAreShown(t *testing.T) {
	// Whatever the server's own address: another doc's file is named by its path.
	oldA := apiBase
	t.Cleanup(func() { apiBase = oldA })
	apiBase = func() string { return "https://somewhere-else.example.test" }
	saved := `<img src="` + att(docID, fileA) + `" alt="Our roadmap" width="640">
<img src="` + att(otherID, fileB) + `" alt="Their secret">
<img src="https://cdn.example/x.png">
<div data-type="file" data-kind="file" data-src="` + att(docID, fileB) + `" data-name="budget.xlsx"></div>
<div data-type="file" data-kind="pdf" data-src="` + att(otherID, fileA) + `" data-name="salaries.pdf"></div>
<div data-type="bookmark" data-url="https://example.org/post" data-title="A post"></div>
<p><a href="` + att(docID, fileA) + `">the roadmap</a> <a href="` + att(otherID, fileB) + `">theirs</a></p>`
	out := pageOf(saved)
	if !strings.Contains(out, `<img src="/public/p/launch-notes/files/`+fileA+`" loading="lazy" alt="Our roadmap" width="640"/>`) {
		t.Errorf("the doc's own picture is served by the page:\n%s", out)
	}
	if strings.Contains(out, otherID) || strings.Contains(out, "cdn.example") || strings.Contains(out, "getDocAttachment") {
		t.Errorf("another doc's file, or a picture from elsewhere, shows:\n%s", out)
	}
	if !strings.Contains(out, "Their secret") || !strings.Contains(out, "salaries.pdf") || !strings.Contains(out, "theirs") {
		t.Errorf("what isn't shown keeps its words:\n%s", out)
	}
	if !strings.Contains(out, `<a href="/public/p/launch-notes/files/`+fileB+`" rel="nofollow noreferrer">budget.xlsx</a>`) {
		t.Errorf("the doc's own file is a link to the page's copy of it:\n%s", out)
	}
	if !strings.Contains(out, `<a href="https://example.org/post" rel="nofollow noreferrer">A post</a>`) {
		t.Errorf("a bookmark links to the web:\n%s", out)
	}

	// A password page's pictures carry its unlock.
	locked := renderer{docUUID: docID, fileURL: fileURL(Ref{Slug: "launch-notes"}, "1.99.sig")}.body(`<img src="` + att(docID, fileA) + `">`)
	if !strings.Contains(locked, `src="/public/p/launch-notes/files/`+fileA+`?k=1.99.sig"`) {
		t.Errorf("a password page's picture opens as the page did:\n%s", locked)
	}
}

// The doc's structure and its colours are drawn by the page's own style.
func TestStructureAndColoursAreThePagesOwn(t *testing.T) {
	saved := `<div data-type="callout" data-icon="💡"><p>Remember</p></div>
<div data-type="column-list"><div data-type="column" data-width="50"><p>Left</p></div><div data-type="column"><p>Right</p></div></div>
<div data-type="collapsible"><h2>Details</h2><p>Hidden at first</p></div>
<ul data-type="taskList"><li data-type="taskItem" data-checked="true"><p>Done</p></li><li data-type="taskItem" data-checked="false"><p>Todo</p></li></ul>
<p>Area <span data-type="inline-math" data-latex="\pi r^2"></span></p><div data-type="block-math" data-latex="E=mc^2"></div>
<div data-type="diff" data-diff="@@ -1 +1 @@&#10;-old&#10;+new"></div>
<p data-block-color="sky_background">Blue band</p><h3 data-block-color="berry">Red heading</h3>
<p><span data-color="moss">green</span> and <mark data-color="sun">marked</mark> and <span data-color="#ff0000">not ours</span></p>
<pre><code class="language-go evil-class">fmt.Println()</code></pre>`
	out := pageOf(saved)
	for _, want := range []string{
		`<div class="callout"><span class="ci">💡</span><div><p>Remember</p></div></div>`,
		`<div class="columns"><div class="column"><p>Left</p></div><div class="column"><p>Right</p></div></div>`,
		`<details class="toggle"><summary><h2>Details</h2></summary><p>Hidden at first</p></details>`,
		`<ul class="tasks"><li class="task done"><span class="box">☑ </span><p>Done</p></li><li class="task"><span class="box">☐ </span><p>Todo</p></li></ul>`,
		`<code class="math">\pi r^2</code>`, `<p><code class="math">E=mc^2</code></p>`,
		`<pre class="diff"><code><span class="dm">@@ -1 +1 @@`, `<span class="dd">-old`, `<span class="da">+new`,
		`<p class="bg-sky">Blue band</p>`, `<h3 class="bc-berry">Red heading</h3>`,
		`<span class="c-moss">green</span>`, `<mark class="h-sun">marked</mark>`, `<span>not ours</span>`,
		`<code class="language-go">fmt.Println()</code>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the page misses %s:\n%s", want, out)
		}
	}
}

// A copy of the page keeps the editor's own form, leaves out what the page
// hides, and names its own files.
func TestACopyKeepsWhatThePageShows(t *testing.T) {
	saved := `<div data-type="callout" data-icon="💡"><p onclick="x()">Remember <span data-type="mention" data-id="uid-1">@Priya</span></p></div>
<div data-type="task-list-embed" data-project="p-1"></div><div data-type="toc"></div>
<ul data-type="taskList"><li data-type="taskItem" data-checked="true"><p>Done</p></li></ul>
<p><span data-color="moss">green</span></p>
<img src="` + att(docID, fileA) + `" alt="Ours" data-align="center"><img src="` + att(otherID, fileB) + `" alt="Theirs">
<img src="` + att(docID, fileB) + `" alt="Not copied">
<div data-type="file" data-kind="file" data-src="` + att(docID, fileA) + `" data-name="a.pdf"><a href="` + att(docID, fileA) + `">a.pdf</a></div>
<script>alert(1)</script>`
	given := map[string]string{fileA: "https://api.example.test/doc/getDocAttachment/copy-doc/new-a"}
	out := renderer{docUUID: docID, fileURL: func(obj string) string { return given[obj] }}.copyOf(saved)
	for _, want := range []string{
		`<div data-type="callout" data-icon="💡"><p>Remember <span>@Priya</span></p></div>`,
		`<div data-type="toc"></div>`,
		`<ul data-type="taskList"><li data-type="taskItem" data-checked="true"><p>Done</p></li></ul>`,
		`<span data-color="moss">green</span>`,
		`<img src="https://api.example.test/doc/getDocAttachment/copy-doc/new-a" alt="Ours" data-align="center"/>`,
		`data-src="https://api.example.test/doc/getDocAttachment/copy-doc/new-a"`,
		`<a href="https://api.example.test/doc/getDocAttachment/copy-doc/new-a">a.pdf</a>`,
		`<span>Theirs</span>`, `<span>Not copied</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the copy misses %s:\n%s", want, out)
		}
	}
	for _, gone := range []string{"task-list-embed", "p-1", "uid-1", "onclick", "<script", otherID, docID} {
		if strings.Contains(out, gone) {
			t.Errorf("the copy keeps %q:\n%s", gone, out)
		}
	}
}

func TestFilesInAreTheDocsOwnOnce(t *testing.T) {
	saved := `<img src="` + att(docID, fileA) + `"><img src="` + att(docID, strings.ToUpper(fileA)) + `">` +
		`<img src="` + att(otherID, fileB) + `"><div data-type="file" data-src="` + att(strings.ToUpper(docID), fileB) + `"></div>`
	got := filesIn(docID, saved)
	if len(got) != 2 || got[0] != fileA || got[1] != fileB {
		t.Errorf("filesIn = %v, want [%s %s]", got, fileA, fileB)
	}
}
