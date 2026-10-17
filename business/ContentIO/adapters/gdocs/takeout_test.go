package gdocs

import (
	"archive/zip"
	"bytes"
	"context"
	"testing"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
)

type collected struct {
	pages   []contentIOBusiness.Page
	skipped []string
}

func (c *collected) Page(p contentIOBusiness.Page) error { c.pages = append(c.pages, p); return nil }
func (c *collected) Progress(int, int)                   {}
func (c *collected) Skip(what, _ string)                 { c.skipped = append(c.skipped, what) }

func archive(t *testing.T, files map[string]string) *contentIOBusiness.ZipArchive {
	t.Helper()
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, data := range files {
		f, _ := zw.Create(name)
		_, _ = f.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	z, err := contentIOBusiness.OpenZip(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	return z
}

// ai-search-io.md P2-6: a Takeout of a Drive makes the Drive's documents into
// docs, folders as pages, without Takeout's index, and says what it left out.
func TestATakeoutOfADriveImportsItsDocuments(t *testing.T) {
	z := archive(t, map[string]string{
		"Takeout/archive_browser.html":   "<html><title>Takeout</title></html>",
		"Takeout/Drive/Plans.md":         "# Plans\n\nShip it.",
		"Takeout/Drive/Team/Notes.txt":   "notes",
		"Takeout/Drive/Team/Budget.xlsx": "xlsx",
	})
	if !(takeout{}).Claims(z.Names()) {
		t.Fatal("the Takeout was not claimed")
	}
	var c collected
	if err := (takeout{}).Read(context.Background(), z, &c); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]contentIOBusiness.Page{}
	for _, p := range c.pages {
		byKey[p.Key] = p
	}
	if len(c.pages) != 3 || byKey["Takeout/Drive/Plans.md"].ParentKey != "" ||
		byKey["Takeout/Drive/Team/Notes.txt"].ParentKey != "Takeout/Drive/Team/" || byKey["Takeout/Drive/Team/"].Title != "Team" {
		t.Fatalf("pages %+v", c.pages)
	}
	if len(c.skipped) != 1 || c.skipped[0] != "Team/Budget.xlsx" {
		t.Fatalf("skipped %v", c.skipped)
	}
}

func TestAPlainZipIsNotATakeout(t *testing.T) {
	if (takeout{}).Claims([]string{"Drive/Plans.md", "Plans.md"}) {
		t.Fatal("a zip without Takeout/Drive was claimed")
	}
}
