package business

import (
	"strings"
	"testing"
)

// These tests pin the injection-safety contract of the external aggregation SQL
// builder: identifiers are only ever accepted when they resolve against the
// live schema (unknown/ambiguous rejected), and filter VALUES always go through
// a bound placeholder ($n) rather than string interpolation. RunAggregate
// itself needs a DB; these cover the pure, security-relevant helpers.

func sampleTables() []TableSchema {
	return []TableSchema{
		{Schema: "public", Name: "deals", Columns: []ColumnSchema{
			{Name: "stage", DataType: "text"},
			{Name: "amount", DataType: "number"},
			{Name: "closed_at", DataType: "date"},
		}},
		{Schema: "sales", Name: "deals", Columns: []ColumnSchema{
			{Name: "owner", DataType: "text"},
		}},
		{Schema: "public", Name: "accounts", Columns: []ColumnSchema{
			{Name: "name", DataType: "text"},
		}},
	}
}

func TestResolveTable(t *testing.T) {
	tables := sampleTables()

	if tbl, err := resolveTable(tables, "public.deals"); err != nil || tbl.Schema != "public" {
		t.Fatalf("schema-qualified resolve failed: %v", err)
	}
	// "accounts" is unique across schemas -> resolvable bare.
	if tbl, err := resolveTable(tables, "accounts"); err != nil || tbl.Name != "accounts" {
		t.Fatalf("unique bare resolve failed: %v", err)
	}
	// "deals" exists in two schemas -> must be rejected as ambiguous.
	if _, err := resolveTable(tables, "deals"); err == nil {
		t.Fatal("expected ambiguous bare table to be rejected")
	}
	// unknown table rejected.
	if _, err := resolveTable(tables, "public.nope"); err == nil {
		t.Fatal("expected unknown table to be rejected")
	}
}

func TestFindColumnCaseInsensitive(t *testing.T) {
	tbl := sampleTables()[0]
	if _, ok := findColumn(&tbl, "AMOUNT"); !ok {
		t.Fatal("expected case-insensitive column match")
	}
	if _, ok := findColumn(&tbl, "missing"); ok {
		t.Fatal("expected unknown column to be rejected")
	}
}

func TestBuildWhereClauseUsesPlaceholders(t *testing.T) {
	col := `"amount"`
	cases := []struct {
		op        string
		dtype     string
		wantParam bool
		contains  string
	}{
		{"eq", "text", true, "$1"},
		{"ne", "text", true, "$1"},
		{"contains", "text", true, "ILIKE '%' || $1 || '%'"},
		{"gt", "number", true, "$1::numeric"},
		{"lte", "date", true, "$1::timestamptz"},
		{"empty", "text", false, "IS NULL"},
		{"not_empty", "text", false, "IS NOT NULL"},
	}
	for _, c := range cases {
		argN := 1
		clause, used := buildWhereClause(postgresDialect{}, col, c.dtype, c.op, "anything'; DROP TABLE x;--", &argN)
		if used != c.wantParam {
			t.Fatalf("op %s: used=%v want %v", c.op, used, c.wantParam)
		}
		if !strings.Contains(clause, c.contains) {
			t.Fatalf("op %s: clause %q missing %q", c.op, clause, c.contains)
		}
		// The raw (malicious) value must NEVER be interpolated into the clause.
		if strings.Contains(clause, "DROP TABLE") {
			t.Fatalf("op %s: value was interpolated into SQL: %q", c.op, clause)
		}
	}
}

// TestDialectParamCasts pins the per-engine casts applied to a bound comparison
// value, so a text parameter compares correctly against a typed column.
func TestDialectParamCasts(t *testing.T) {
	pg := postgresDialect{}
	if pg.CastNumericParam("$1") != "$1::numeric" {
		t.Fatalf("pg numeric cast: %s", pg.CastNumericParam("$1"))
	}
	if pg.CastDateParam("$1") != "$1::timestamptz" {
		t.Fatalf("pg date cast: %s", pg.CastDateParam("$1"))
	}
	my := mysqlDialect{}
	if my.CastNumericParam("?") != "CAST(? AS DECIMAL(65,10))" {
		t.Fatalf("mysql numeric cast: %s", my.CastNumericParam("?"))
	}
	if my.CastDateParam("?") != "CAST(? AS DATETIME)" {
		t.Fatalf("mysql date cast: %s", my.CastDateParam("?"))
	}
}
