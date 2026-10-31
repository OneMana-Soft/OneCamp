package business

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"

	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	"github.com/google/uuid"
)

// An admin opens the audit export in a spreadsheet, and its rows hold text
// other people wrote: a summary quoting what they typed, the email a sign-in
// was tried with, the metadata a client sent. A spreadsheet runs a cell that
// starts with = + - or @ as a formula as the file opens, so each such cell
// reads as text. The byte order mark comes first, so Excel reads "Zoë" as
// typed. The chain's hashes stay as written: each row still links to the one
// before it.
func TestEntriesToCSVOpensAsTextInASpreadsheet(t *testing.T) {
	meta := `{"title":"=1+1"}`
	entries := []*auditModel.AuditEntry{
		{
			Id: uuid.MustParse("6f1c2b3a-0000-4000-8000-000000000001"), Seq: 7,
			CreatedAt:  time.Date(2026, 10, 24, 5, 30, 0, 0, time.UTC),
			ActorEmail: "-zoë@example.com", Action: "auth.login_failed", Category: "security",
			Summary: `=HYPERLINK("https://example.com/x","Open")`, IPAddress: "203.0.113.9",
			EntryHash: "9f2c", PrevHash: "41ab",
		},
		{
			Id: uuid.MustParse("6f1c2b3a-0000-4000-8000-000000000002"), Seq: 8,
			CreatedAt:  time.Date(2026, 10, 24, 5, 31, 0, 0, time.UTC),
			ActorEmail: "zoë@example.com", Action: "doc.renamed", Category: "content",
			Summary: "@here the launch plan", Metadata: &meta, IPAddress: "2001:db8::1",
			EntryHash: "c07e", PrevHash: "9f2c",
		},
	}

	out, err := EntriesToCSV(entries)
	if err != nil {
		t.Fatal(err)
	}
	const bom = "\uFEFF"
	if !bytes.HasPrefix(out, []byte(bom)) {
		t.Errorf("the export starts %q, want the byte order mark first", out[:min(len(out), 6)])
	}
	rows, err := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(out, []byte(bom)))).ReadAll()
	if err != nil {
		t.Fatalf("the export doesn't read back as CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("the export has %d rows, want the header and two entries", len(rows))
	}
	col := map[string]int{}
	for i, name := range rows[0] {
		col[name] = i
	}
	for _, c := range []struct {
		row        int
		name, want string
	}{
		{1, "actor_email", "'-zoë@example.com"},
		{1, "summary", `'=HYPERLINK("https://example.com/x","Open")`},
		{2, "summary", "'@here the launch plan"},
		// Text that doesn't start like a formula is left as it was written.
		{2, "actor_email", "zoë@example.com"},
		{2, "metadata", meta},
		{2, "ip_address", "2001:db8::1"},
		{1, "entry_hash", "9f2c"},
		{2, "prev_hash", "9f2c"},
	} {
		if got := rows[c.row][col[c.name]]; got != c.want {
			t.Errorf("row %d's %s reads %q, want %q", c.row, c.name, got, c.want)
		}
	}
}
