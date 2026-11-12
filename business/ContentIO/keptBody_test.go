package business

import (
	"strings"
	"testing"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
)

// keptWhiteboardBody is a stored body the collaboration service wrote while
// the web was a release ahead of it: the whiteboard block is a marker
// (preserved.js). V-528 (1): every export, and the API's GET (the Markdown
// export), keeps the block.
const keptWhiteboardBody = `<p>Before</p><div data-oc-preserved="whiteboardEmbed" data-oc-holds="none" data-oc-attrs="{&quot;boardId&quot;:&quot;0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d&quot;}"></div><p>After</p>`

func TestExportsKeepABlockTheServiceKept(t *testing.T) {
	// The read every export makes of a doc's body (docExporter.Export,
	// the workspace export).
	body, err := contentcodec.FromHTML(keptWhiteboardBody, contentcodec.HTMLOptions{})
	if err != nil {
		t.Fatal(err)
	}
	md := docMarkdown("Plan", body, "https://app.example.test")
	if !strings.Contains(md, "https://app.example.test/app/board/0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d") {
		t.Errorf("Markdown export leaves the whiteboard out:\n%s", md)
	}
	if page := docPage("Plan", body); !strings.Contains(page, `data-type="whiteboard-embed" data-board-id="0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d"`) {
		t.Errorf("HTML export leaves the whiteboard out:\n%s", page)
	}
	raw, err := docJSON("Plan", body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"whiteboardEmbed"`) {
		t.Errorf("JSON export leaves the whiteboard out:\n%s", raw)
	}
}
