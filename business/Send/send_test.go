package business

import (
	"net/http"
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// The rule for writing in a channel, which a forward asks as a post does.
func TestMayPostIn(t *testing.T) {
	live, archived := time.Time{}, time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name string
		ch   *dgraphStruct.DgraphChannel
		want int // 0: allowed
	}{
		{"no channel", nil, http.StatusBadRequest},
		{"an archived channel", &dgraphStruct.DgraphChannel{IsMember: 1, IsAdmin: 1, DeletedAt: &archived}, http.StatusBadRequest},
		{"a channel they're not in", &dgraphStruct.DgraphChannel{DeletedAt: &live}, http.StatusForbidden},
		{"their channel", &dgraphStruct.DgraphChannel{IsMember: 1, DeletedAt: &live}, 0},
		{"their channel, read without its deletion time", &dgraphStruct.DgraphChannel{IsMember: 1}, 0},
		{"an announcement channel they don't run", &dgraphStruct.DgraphChannel{IsMember: 1, PostPolicy: "admins_only", DeletedAt: &live}, http.StatusForbidden},
		{"an announcement channel they run", &dgraphStruct.DgraphChannel{IsMember: 1, IsAdmin: 1, PostPolicy: "admins_only", DeletedAt: &live}, 0},
		{"an announcement channel they run but aren't in", &dgraphStruct.DgraphChannel{IsAdmin: 1, PostPolicy: "admins_only", DeletedAt: &live}, http.StatusForbidden},
	} {
		rj := MayPostIn(c.ch)
		switch {
		case c.want == 0 && rj != nil:
			t.Errorf("%s: refused (%d %s)", c.name, rj.Status, rj.Msg)
		case c.want != 0 && (rj == nil || rj.Status != c.want):
			t.Errorf("%s: %+v, want a %d", c.name, rj, c.want)
		}
	}
}

// The rule for writing to a person, which a forward asks as a direct message does.
func TestMayMessage(t *testing.T) {
	live, gone := time.Time{}, time.Now().Add(-time.Hour)
	for _, c := range []struct {
		name string
		to   *dgraphStruct.DgraphUser
		ok   bool
	}{
		{"no one", nil, false},
		{"someone", &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &live}, true},
		{"someone read without a deletion time", &dgraphStruct.DgraphUser{Uid: "0x1"}, true},
		{"a deactivated account", &dgraphStruct.DgraphUser{Uid: "0x1", DeletedAt: &gone}, false},
		{"an identity nobody signs in as", &dgraphStruct.DgraphUser{Uid: "0x1", IsExternal: true}, false},
		{"a bot", &dgraphStruct.DgraphUser{Uid: "0x1", IsExternal: true, IsBot: true}, true},
	} {
		rj := MayMessage(c.to)
		if (rj == nil) != c.ok {
			t.Errorf("%s: %+v, want allowed=%v", c.name, rj, c.ok)
		}
		if rj != nil && rj.Status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want the 400 a direct message gets", c.name, rj.Status)
		}
	}
}
