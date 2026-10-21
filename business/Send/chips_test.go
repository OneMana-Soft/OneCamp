package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// A chip says who it notifies (MC-91): the graph's uuid and name for the uid
// it names, whatever the sender wrote; anything else is left as it is.
func TestAChipSaysWhoItNotifies(t *testing.T) {
	maya := &dgraphStruct.DgraphUser{Uid: "0x2a", Uuid: "u-maya", UserName: "Maya <Chen>"}
	in := `<p>hi <span class="mention" data-id="u-forged@0x2a" data-label="Boss" data-type="mention">@Boss</span> and ` +
		`<span data-type="mention" data-id="broadcast:here" data-label="here">@here</span> and ` +
		`<span data-type="mention" data-id="u-x@0x99" data-label="X">@X</span></p>`
	got := normaliseChips(in, []*dgraphStruct.DgraphUser{maya})
	want := `<p>hi <span class="mention" data-id="u-maya@0x2a" data-label="Maya &lt;Chen&gt;" data-type="mention">@Maya &lt;Chen&gt;</span> and ` +
		`<span data-type="mention" data-id="broadcast:here" data-label="here">@here</span> and ` +
		`<span data-type="mention" data-id="u-x@0x99" data-label="X">@X</span></p>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if normaliseChips("<p>plain</p>", []*dgraphStruct.DgraphUser{maya}) != "<p>plain</p>" {
		t.Error("plain words changed")
	}
}
