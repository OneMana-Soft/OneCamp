package controller

import (
	"errors"
	"testing"
)

// A part that fails says so in its place, and the task and the other parts
// stand: the panel shows that part's own error (TASKPANEL-FIX).
func TestAFailedPartSaysSoInItsPlace(t *testing.T) {
	ok := partOf("cycle", map[string]int{"n": 1}, nil)
	if _, has := ok["data"]; !has || ok["error"] != nil {
		t.Fatalf("a part that came is %+v", ok)
	}
	bad := partOf("client_review", nil, errors.New("pq: connection refused"))
	if _, has := bad["data"]; has || bad["error"] != "Couldn't read the client review. Try again." {
		t.Fatalf("a part that failed is %+v", bad)
	}
}
