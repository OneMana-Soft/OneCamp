package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// Who a post names by their own chip, for its email: never someone a
// broadcast or a team's mention reached, and matched by the graph uid, never
// the uuid the sender's chip claims.
func TestEmailGoesToThePeopleAPostNames(t *testing.T) {
	jonas := &dgraphStruct.DgraphUser{Uid: "0x2", Uuid: "u-jonas"}
	sam := &dgraphStruct.DgraphUser{Uid: "0x3", Uuid: "u-sam"}
	html := `<p><span data-type="mention" data-id="u-forged@0x2">@Jonas</span> ` +
		`<span data-type="mention" data-id="broadcast:channel">@channel</span></p>`
	named := namedIn(html, []*dgraphStruct.DgraphUser{jonas, sam, nil})
	if !named["u-jonas"] || named["u-sam"] || len(named) != 1 {
		t.Errorf("named %v: Jonas, by his uid, and not Sam, whom @channel reached", named)
	}
	if got := namedIn("", []*dgraphStruct.DgraphUser{jonas}); len(got) != 0 {
		t.Errorf("no words named %v", got)
	}
}
