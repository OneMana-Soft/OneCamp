// Package gdocs reads Google Docs into docs. Google Docs come across as Word
// files (File > Download > Microsoft Word, or Drive's own download of a
// folder), which the pipeline's Word importer reads; what this adds is
// Google Takeout's zip of a whole Drive.
//
// A Takeout zip holds the Drive under Takeout/Drive/, with each Google Doc
// as a .docx by default, beside Takeout's own index (archive_browser.html)
// and whatever else the Drive held. Only the Drive's documents are read: the
// index would otherwise become a doc, and spreadsheets and slides are said to
// be left out rather than dropped silently.
//
// Sources, read 14 Oct 2026: Google's Drive export formats as Takeout
// documents them (Docs as DOCX unless chosen otherwise), and the archive's
// layout, Takeout/<product>/, with archive_browser.html at its top.
package gdocs

import (
	"context"
	"path"
	"strings"

	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
)

func init() { contentIOBusiness.RegisterZipLayout(takeout{}) }

const drive = "Takeout/Drive/"

// officeFiles are what a Drive holds besides documents, which no doc is made
// of.
var officeFiles = map[string]string{
	".xlsx": "a spreadsheet", ".ods": "a spreadsheet", ".csv": "a spreadsheet",
	".pptx": "a presentation", ".odp": "a presentation", ".pdf": "a PDF",
}

type takeout struct{}

func (takeout) Name() string { return "google-takeout" }

func (takeout) Claims(names []string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, drive) {
			return true
		}
	}
	return false
}

func (takeout) Read(ctx context.Context, z *contentIOBusiness.ZipArchive, out contentIOBusiness.Output) error {
	for _, n := range z.Names() {
		if kind, ok := officeFiles[strings.ToLower(path.Ext(n))]; ok && strings.HasPrefix(n, drive) {
			out.Skip(strings.TrimPrefix(n, drive), "it is "+kind+"; only documents are imported as docs")
		}
	}
	return z.Without(func(n string) bool { return !strings.HasPrefix(n, drive) }).Pages(ctx, out, nil)
}
