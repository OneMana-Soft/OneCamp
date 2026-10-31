package models

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The t index holds only the texts a btree entry can (T5c), and a query
// reads it only by saying so in its predicate's own words (ShortText): a
// different bound in either would leave the index unread, or a long text
// unsaveable. The migration is found by its name, as the integrator numbers
// it at landing.
func TestTheTIndexAndItsReadersSayTheSameWords(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(file), "..", "..", "..", "migrations", "*_table_sort_keys.up.sql"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the migration: %v %v", matches, err)
	}
	up, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := "WHERE t IS NOT NULL AND " + ShortText("t") + ";"; !strings.Contains(string(up), want) {
		t.Errorf("the t index's predicate isn't %q", want)
	}
	// What the index holds fits a btree entry with its field, place and row:
	// 2,704 bytes at most.
	if IndexedTextBytes+16+16+600 > 2704 {
		t.Errorf("%d bytes of t, with a place of up to 600, don't fit a btree entry", IndexedTextBytes)
	}
}
