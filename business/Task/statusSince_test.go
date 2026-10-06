package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestStatusSinceOf(t *testing.T) {
	at := func(d int) *time.Time { v := time.Date(2026, 9, d, 10, 0, 0, 0, time.UTC); return &v }
	now := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	status := func(d int) *dgraphStruct.DgraphTaskActivity {
		return &dgraphStruct.DgraphTaskActivity{Type: dgraphStruct.ACTIVITY_TYPE_STATUS, LogTime: at(d)}
	}
	cases := []struct {
		name string
		task dgraphStruct.DgraphTask
		want time.Time
	}{
		{"its latest status change", dgraphStruct.DgraphTask{CreatedAt: at(1), Activity: []*dgraphStruct.DgraphTaskActivity{status(3), status(9), status(5)}}, *at(9)},
		{"other activity does not count", dgraphStruct.DgraphTask{CreatedAt: at(1), Activity: []*dgraphStruct.DgraphTaskActivity{{Type: "priorityUpdate", LogTime: at(20)}}}, *at(1)},
		{"created and never moved", dgraphStruct.DgraphTask{CreatedAt: at(2)}, *at(2)},
		{"nothing known", dgraphStruct.DgraphTask{CreatedAt: &time.Time{}}, now},
	}
	for _, c := range cases {
		if got := StatusSinceOf(&c.task, now); !got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
