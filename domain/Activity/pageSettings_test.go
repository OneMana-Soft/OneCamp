package domain

import (
	"reflect"
	"testing"
)

// The page's one statement answers held notices and settings as rows of one
// shape; each lands by its kind and place alone. A held row whose key reads
// like a channel's id is no setting, a setting whose id reads like a stored key
// holds no notice, and a page that asked only one side gets nothing on the
// other (MAIN 16:3x, NOTIF M1 store budget).
func TestAPageSettingLandsByItsKindNeverByItsShape(t *testing.T) {
	const ch = "0c1d2e3f-0000-4000-8000-000000000001"
	asked := map[string]string{"k-stored": "k-asked", ch: ch}
	cases := []struct {
		name string
		rows []pageRow
		want PageSettings
	}{
		{"held keys and no places", []pageRow{{kind: "held", id: "k-stored"}, {kind: "held", id: ch}},
			PageSettings{Held: map[string]bool{"k-asked": true, ch: true}, ByChannel: map[string]string{}, ByChat: map[string]string{}}},
		{"places and no held keys", []pageRow{{kind: "level", place: "channel", id: ch, level: "block"}, {kind: "level", place: "chat", id: "k-stored", level: "mention"}},
			PageSettings{Held: map[string]bool{}, ByChannel: map[string]string{ch: "block"}, ByChat: map[string]string{"k-stored": "mention"}}},
		{"both, a row of no known kind or place dropped", []pageRow{
			{kind: "held", id: "k-stored"}, {kind: "level", place: "channel", id: ch, level: "all"},
			{kind: "level", place: "", id: "x", level: "block"}, {kind: "", place: "channel", id: "y", level: "block"},
			{kind: "held", id: "never-asked"},
		}, PageSettings{Held: map[string]bool{"k-asked": true}, ByChannel: map[string]string{ch: "all"}, ByChat: map[string]string{}}},
		{"nothing", nil, PageSettings{Held: map[string]bool{}, ByChannel: map[string]string{}, ByChat: map[string]string{}}},
	}
	for _, c := range cases {
		if got := pageSettingsOf(c.rows, asked); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}
