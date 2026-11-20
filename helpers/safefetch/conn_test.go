package safefetch

import (
	"errors"
	"io"
	"net"
	"testing"
)

// A conn under LimitReads reads its budget and no more: the read past it
// fails with ErrReadBudget and closes the conn.
func TestLimitReadsStopsAtTheBudget(t *testing.T) {
	far, near := net.Pipe()
	defer far.Close()
	go func() { _, _ = far.Write(make([]byte, 64)) }()
	c := LimitReads(near, 10)
	got, err := io.ReadAll(c)
	if len(got) != 10 || !errors.Is(err, ErrReadBudget) {
		t.Fatalf("read %d bytes, err %v: want 10, then ErrReadBudget", len(got), err)
	}
	if _, err := near.Read(make([]byte, 1)); err == nil {
		t.Fatal("the conn is still open past its budget")
	}
}
