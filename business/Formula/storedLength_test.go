package formula

import (
	"errors"
	"strings"
	"testing"
)

// People write at most MaxLength characters. The stored form (Canonical:
// each {Field name} written as the field's id) is longer, and a read of it
// still works: J-F's TestJFAReadHasOneBudget read "A formula can be at most
// 2000 characters" from a formula written in 1,959.
func TestAStoredFormulaLongerThanItsWrittenFormReads(t *testing.T) {
	id := strings.Repeat("p", 36) // as long as a uuid
	fields := []Field{{ID: id, Name: "P", Kind: KindNumber}}
	written := strings.TrimSuffix(strings.Repeat("{P}+", 400), "+") // 1,599 characters
	if _, err := Check(written, fields, "f"); err != nil {
		t.Fatalf("the written form: %v", err)
	}
	stored, err := Canonical(written, fields)
	if err != nil || len(stored) <= MaxLength {
		t.Fatalf("the stored form is %d characters (%v), want over %d", len(stored), err, MaxLength)
	}
	p := Compile(append(fields, Field{ID: "f", Name: "F", IsFormula: true, Formula: stored}))
	got := p.Run(func(string) (Value, int) { return Number(1), 0 }, now, ist)["f"]
	if got.Kind != KindNumber || got.Num != 400 {
		t.Fatalf("the stored form reads %+v, want 400", got)
	}
	var se *SyntaxError
	if _, err := Check(strings.Repeat("1+", 1001)+"1", fields, "f"); !errors.As(err, &se) || se.Msg != "A formula can be at most 2000 characters" {
		t.Fatalf("a written form over %d characters: %v", MaxLength, err)
	}
}
