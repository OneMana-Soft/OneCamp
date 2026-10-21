package business

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	"github.com/google/uuid"
)

// N1: a file a page holds (a PDF, a video, a sheet) comes with the import and
// is uploaded as the doc's attachment, as a picture is; one the import doesn't
// have, or that can't be stored, is left out and said so. A file on the web
// keeps its address.
func TestAnImportsFileBlocksAreUploadedWithTheirPage(t *testing.T) {
	saved := uploadFile
	t.Cleanup(func() { uploadFile = saved })
	var stored []string
	uploadFile = func(_ context.Context, _ uuid.UUID, docUUID, name string, r io.Reader, _ int64) (string, string, string, error) {
		data, _ := io.ReadAll(r)
		if string(data) == "refused" {
			return "", "", "", errors.New("blocked")
		}
		stored = append(stored, name+"="+string(data))
		return "obj-1", "https://api.example.test/doc/getDocAttachment/" + docUUID + "/obj-1", "application/pdf", nil
	}
	html := `<div data-type="file" data-kind="pdf" data-src="files/a.pdf" data-name="Plan.pdf"><a href="files/a.pdf">Plan.pdf</a></div>` +
		`<div data-type="file" data-kind="video" data-src="https://example.com/intro.mp4" data-name="intro.mp4"></div>` +
		`<div data-type="file" data-kind="file" data-src="files/gone.xlsx" data-name="Gone.xlsx"></div>` +
		`<div data-type="file" data-kind="file" data-src="files/bad.bin" data-name="Bad.bin"></div>`
	body, err := contentcodec.FromHTML(html, contentcodec.HTMLOptions{Foreign: true, Assets: true})
	if err != nil {
		t.Fatal(err)
	}
	m := &docMaker{userID: uuid.New()}
	pg := Page{Key: "Handbook", Assets: FileAssets{"files/a.pdf": []byte("%PDF plan"), "files/bad.bin": []byte("refused")}}
	out, lost := m.uploadImages(context.Background(), "doc-1", pg, body)
	if len(out.Content) != 2 {
		t.Fatalf("blocks %+v", out.Content)
	}
	pdf, video := out.Content[0].Attrs, out.Content[1].Attrs
	if pdf["src"] != "https://api.example.test/doc/getDocAttachment/doc-1/obj-1" || pdf["id"] != "obj-1" || pdf["name"] != "Plan.pdf" ||
		pdf["mime"] != "application/pdf" || pdf["size"] != float64(9) || pdf["kind"] != "pdf" {
		t.Fatalf("the PDF %+v", pdf)
	}
	if video["src"] != "https://example.com/intro.mp4" {
		t.Fatalf("a file on the web %+v", video)
	}
	if strings.Join(stored, ",") != "Plan.pdf=%PDF plan" || len(lost) != 2 {
		t.Fatalf("stored %v, lost %+v", stored, lost)
	}
}

// Outside an import, a file block never names a relative path: pasted or
// stored HTML keeps only an address that loads.
func TestOnlyAnImportsFileBlocksNameItsFiles(t *testing.T) {
	html := `<div data-type="file" data-kind="pdf" data-src="files/a.pdf" data-name="Plan.pdf"></div>`
	for _, opts := range []contentcodec.HTMLOptions{{Foreign: true}, {}, {Assets: true}} {
		body, _ := contentcodec.FromHTML(html, opts)
		body.Walk(func(n *contentcodec.Node) bool {
			if n.Type == "fileBlock" && n.Attrs["src"] != nil {
				t.Errorf("%+v kept %v", opts, n.Attrs["src"])
			}
			return true
		})
	}
	for _, bad := range []string{"../secret", "//evil.example/x", "javascript:alert(1)", `files\a.pdf`} {
		body, _ := contentcodec.FromHTML(`<div data-type="file" data-src="`+bad+`" data-name="x"></div>`, contentcodec.HTMLOptions{Foreign: true, Assets: true})
		body.Walk(func(n *contentcodec.Node) bool {
			if n.Type == "fileBlock" && n.Attrs["src"] == bad {
				t.Errorf("an import kept %q", bad)
			}
			return true
		})
	}
}
