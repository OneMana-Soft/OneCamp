package models

import (
	"strings"
	"testing"
)

// countingScanner records how many targets a scan asks for.
type countingScanner struct{ n int }

func (c *countingScanner) Scan(dest ...any) error { c.n = len(dest); return nil }

// The scanner must ask for exactly one target per column in allColumns, or
// every query that selects allColumns fails at run time.
func TestScanRowMatchesColumns(t *testing.T) {
	cols := len(strings.Split(allColumns, ","))
	var c countingScanner
	if _, err := scanRow(&c); err != nil {
		t.Fatal(err)
	}
	if c.n != cols {
		t.Fatalf("scanRow reads %d targets, allColumns has %d", c.n, cols)
	}
}
