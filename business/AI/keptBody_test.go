package business

import (
	"strings"
	"testing"
)

// V-528 (1): a body the collaboration service wrote while the web was a
// release ahead of it keeps the whiteboard block as a marker (preserved.js);
// what a model reads of the doc still names it.
func TestDocMarkdownKeepsABlockTheServiceKept(t *testing.T) {
	body := `<p>Before</p><div data-oc-preserved="whiteboardEmbed" data-oc-holds="none" data-oc-attrs="{&quot;boardId&quot;:&quot;0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d&quot;}"></div><p>After</p>`
	if md := DocMarkdown(body); !strings.Contains(md, "/app/board/0b0e5a8e-3c1d-4f6a-9b2e-7d4c5a6b8c9d") {
		t.Errorf("the doc as a model reads it leaves the whiteboard out:\n%s", md)
	}
}
