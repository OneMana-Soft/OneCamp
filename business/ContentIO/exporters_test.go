package business

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
)

var update = flag.Bool("update", false, "write testdata/codec/export from the exporters")

// The web app exports the same files from the live doc (lib/export): both
// are held to testdata/codec/export, byte for byte.
func TestExportsAreTheFilesTheWebAppWrites(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "codec")
	var idx struct {
		AppURL string `json:"appURL"`
		Export []struct {
			Name    string `json:"name"`
			Fixture string `json:"fixture"`
			Title   string `json:"title"`
		} `json:"export"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatal(err)
	}
	if len(idx.Export) == 0 {
		t.Fatal("fixtures.json lists no exports")
	}
	for _, e := range idx.Export {
		src, err := os.ReadFile(filepath.Join(dir, e.Fixture+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var body contentcodec.Node
		if err := json.Unmarshal(src, &body); err != nil {
			t.Fatal(err)
		}
		doc := contentcodec.Normalize(&body, nil)
		files := map[string]string{
			e.Name + ".md":   docMarkdown(e.Title, doc, idx.AppURL),
			e.Name + ".html": docPage(e.Title, doc),
		}
		for name, got := range files {
			path := filepath.Join(dir, "export", name)
			if *update {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal([]byte(got), want) {
				t.Errorf("export/%s differs:\n--- got\n%s\n--- want\n%s", name, got, want)
			}
		}
	}
}
