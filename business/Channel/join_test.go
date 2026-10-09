package business

import (
	"testing"
	"time"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestOnlyALivePublicChannelCanBeJoinedByItself(t *testing.T) {
	public, private := false, true
	zero, archived := time.Time{}, time.Now().Add(-time.Hour)
	for name, tc := range map[string]struct {
		ch   *dgraphStruct.DgraphChannel
		want error
	}{
		"public":          {&dgraphStruct.DgraphChannel{IsPrivate: &public, DeletedAt: &zero}, nil},
		"public, no date": {&dgraphStruct.DgraphChannel{IsPrivate: &public}, nil},
		"private":         {&dgraphStruct.DgraphChannel{IsPrivate: &private, DeletedAt: &zero}, ErrJoinPrivate},
		"privacy unknown": {&dgraphStruct.DgraphChannel{DeletedAt: &zero}, ErrJoinPrivate},
		"archived":        {&dgraphStruct.DgraphChannel{IsPrivate: &public, DeletedAt: &archived}, ErrJoinArchived},
		"missing":         {nil, ErrJoinMissing},
	} {
		if got := CanJoin(tc.ch); got != tc.want {
			t.Errorf("%s: CanJoin = %v, want %v", name, got, tc.want)
		}
	}
}
