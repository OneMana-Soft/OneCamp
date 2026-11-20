package domain

import "testing"

// Each direction's block is the one its page's own copy wrote before the
// copies became one (D5): the same filter, the same order, the same first.
func TestEachDirectionReadsAsItsOwnPageDid(t *testing.T) {
	live := `not gt(chat_deleted_at, "1970-01-01T00:00:00Z")`
	cases := map[Direction]string{
		Latest:    `@filter(` + live + `)(first: $first, orderdesc: chat_created_at)`,
		Older:     `@filter(lt(chat_created_at, $time) AND ` + live + `)(first: $first, orderdesc: chat_created_at)`,
		Newer:     `@filter(gt(chat_created_at, $time) AND ` + live + `)(first: $first, orderasc: chat_created_at)`,
		NewerFrom: `@filter(ge(chat_created_at, $time) AND ` + live + `)(first: $first, orderasc: chat_created_at)`,
	}
	for d, want := range cases {
		if got := d.Block("chat_created_at", "chat_deleted_at"); got != want {
			t.Errorf("direction %d:\n got %s\nwant %s", d, got, want)
		}
	}
}
