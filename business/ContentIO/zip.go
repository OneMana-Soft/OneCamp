package business

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	"github.com/akashc777/OneCamp/helpers/zipsafe"
	"golang.org/x/text/encoding/charmap"
)

// The zip importer: a folder of Markdown, HTML, text and Word files, with the
// images they use, as Notion, Confluence-style tools and people's own folders
// export them. Every page file becomes a doc; a folder becomes the page of the
// file with its name ("Plans.md" beside "Plans/"), or a doc of its own named
// after it; an image a page names is uploaded with it.
//
// A tool whose export needs more than that (its own tree, its icons, its
// tables) reads its zip through a ZipLayout (zipLayouts.go), which claims the
// zip by the names in it.

func init() { Pipeline.RegisterImporter(zipImporter{}) }

const (
	// maxZipEntries is the most files a zip may hold, its pictures and
	// attachments with its pages: a wiki's export holds more pictures than
	// pages.
	maxZipEntries = 50000
	// maxZipPages is the most pages a zip may bring, each one a doc.
	maxZipPages = 5000
	// maxZipUnpacked bounds everything read out of one zip, against a file
	// that unpacks to far more than it weighs.
	maxZipUnpacked = 1 << 30
	// maxZipDirectory bounds a zip's list of files, which is read before its
	// files can be counted: maxZipEntries of a wiki's long paths fit with
	// room to spare.
	maxZipDirectory = 32 << 20
)

// zipLimits is how a zip an import reads is opened (zipsafe.Open): its count
// of files and its directory asked before archive/zip reads the directory.
var zipLimits = zipsafe.Limits{MaxEntries: maxZipEntries, MaxDirectoryBytes: maxZipDirectory}

// zipOpenError is what a zip that can't be opened (zipsafe.Open) is answered
// with: too many files for an import, or not a zip that can be read. what
// names it: "The zip", "The file", or a zip inside one.
func zipOpenError(what string, err error) error {
	if errors.Is(err, zipsafe.ErrTooManyEntries) || errors.Is(err, zipsafe.ErrDirectoryTooLarge) {
		return readError{fmt.Sprintf("%s holds more files than an import reads (%d at most).", what, maxZipEntries)}
	}
	return readError{fmt.Sprintf("%s is not a zip file that can be read.", what)}
}

type zipImporter struct{}

func (zipImporter) Format() string { return "zip" }

func (zipImporter) Extensions() []string { return []string{".zip"} }

func (zipImporter) Accepts(filename, mediaType string) bool {
	t := baseMediaType(mediaType)
	return hasExt(filename, ".zip") || t == "application/zip" || t == "application/x-zip-compressed"
}

func (zipImporter) Import(ctx context.Context, in Input, out Output) error {
	z, err := OpenZip(in.File, in.Size)
	if err != nil {
		return err
	}
	defer z.Close()
	if layout := layoutFor(z.Names()); layout != nil {
		return layout.Read(ctx, z, out)
	}
	return z.Pages(ctx, out, nil)
}

// ZipArchive is an import's zip: the files in it that anyone meant to import
// (no folders, no macOS forks, no dot-files), read within one budget for the
// whole zip.
type ZipArchive struct {
	files  map[string]*zip.File
	names  []string
	budget *unpackBudget
	// parts are the zips at its top, each unpacked to a file of its own while
	// the archive is read; Close removes them.
	parts []*os.File
}

// OpenZip opens a zip for an import. The zips at its top are read as part of
// it: Notion splits a large export into Export-…-Part-1.zip, Part-2.zip and
// so on inside the zip it gives (obsidian-importer reads it the same way). A
// zip deeper in is a file like any other. Close removes what reading the
// parts left on disk.
func OpenZip(r io.ReaderAt, size int64) (*ZipArchive, error) {
	zr, err := zipsafe.Open(r, size, zipLimits)
	if err != nil {
		if errors.Is(err, zipsafe.ErrTooManyEntries) || errors.Is(err, zipsafe.ErrDirectoryTooLarge) {
			return nil, zipOpenError("The zip", err)
		}
		return nil, readError{"This is not a zip file that can be read."}
	}
	z := &ZipArchive{files: map[string]*zip.File{}, budget: &unpackBudget{left: maxZipUnpacked}}
	if err := z.add(zr, true); err != nil {
		z.Close()
		return nil, err
	}
	sort.Strings(z.names)
	return z, nil
}

// openPackage opens a zip that is one file, its parts named as written in it
// (a Word file: word/document.xml, word/media/…), unpacked within budget:
// the import's zip's when it sits in one, else one of its own. Every zip an
// import reads is unpacked by the one rule (unpackBudget).
func openPackage(r io.ReaderAt, size int64, budget *unpackBudget) (*ZipArchive, error) {
	zr, err := zipsafe.Open(r, size, zipLimits)
	if errors.Is(err, zipsafe.ErrTooManyEntries) || errors.Is(err, zipsafe.ErrDirectoryTooLarge) {
		return nil, zipOpenError("The file", err)
	}
	if err != nil {
		return nil, err
	}
	if len(zr.File) > maxZipEntries {
		return nil, readError{fmt.Sprintf("The file holds %d parts; an import reads at most %d.", len(zr.File), maxZipEntries)}
	}
	if budget == nil {
		budget = &unpackBudget{left: maxZipUnpacked}
	}
	z := &ZipArchive{files: make(map[string]*zip.File, len(zr.File)), budget: budget}
	for _, f := range zr.File {
		z.files[f.Name] = f
	}
	return z, nil
}

// add takes a zip's files into the archive, and with parts the files of the
// zips at its top.
func (z *ZipArchive) add(zr *zip.Reader, parts bool) error {
	if len(zr.File) > maxZipEntries {
		return readError{fmt.Sprintf("The zip holds %d files; an import reads at most %d.", len(zr.File), maxZipEntries)}
	}
	for _, f := range zr.File {
		name := cleanZipPath(zipName(f))
		if name == "" || f.FileInfo().IsDir() || hiddenZipPath(name) {
			continue
		}
		if parts && !strings.Contains(name, "/") && hasExt(name, ".zip") {
			if err := z.addPart(f); err != nil {
				return err
			}
			continue
		}
		if _, ok := z.files[name]; !ok {
			z.names = append(z.names, name)
		}
		z.files[name] = f
	}
	if len(z.names) > maxZipEntries {
		return readError{fmt.Sprintf("The zip holds %d files; an import reads at most %d.", len(z.names), maxZipEntries)}
	}
	return nil
}

// addPart unpacks a zip at the top of the zip to a file of its own, within the
// budget, and takes its files in.
func (z *ZipArchive) addPart(f *zip.File) error {
	tmp, size, err := z.budget.spill(f)
	if err != nil {
		return err
	}
	z.parts = append(z.parts, tmp)
	zr, err := zipsafe.Open(tmp, size, zipLimits)
	if err != nil {
		return zipOpenError(path.Base(f.Name)+", inside the zip,", err)
	}
	return z.add(zr, false)
}

// Close removes the files the zips at its top were unpacked to.
func (z *ZipArchive) Close() {
	for _, f := range z.parts {
		f.Close()
		os.Remove(f.Name())
	}
	z.parts = nil
}

// Names are the zip's files, sorted.
func (z *ZipArchive) Names() []string { return z.names }

// Read reads one file whole, failing past max bytes or the zip's budget.
func (z *ZipArchive) Read(name string, max int64) ([]byte, error) {
	f := z.files[name]
	if f == nil {
		return nil, ErrNotFound
	}
	return z.budget.read(f, max)
}

// ReadText reads a page file whole as text (DecodeText), failing past a
// page's size or the zip's budget.
func (z *ZipArchive) ReadText(name string) (string, error) {
	data, err := z.Read(name, maxTextBytes)
	if err != nil {
		return "", err
	}
	return DecodeText(data), nil
}

// Assets opens the zip's files for the pages in it, by their path in the zip.
func (z *ZipArchive) Assets() AssetOpener { return zipAssets{files: z.files, budget: z.budget} }

// Without is the zip without the files skip names.
func (z *ZipArchive) Without(skip func(name string) bool) *ZipArchive {
	out := &ZipArchive{files: map[string]*zip.File{}, budget: z.budget}
	for _, n := range z.names {
		if !skip(n) {
			out.files[n] = z.files[n]
			out.names = append(out.names, n)
		}
	}
	return out
}

// PageReader reads one Markdown, HTML or text file of a zip, given as text;
// nil reads it as its extension says.
type PageReader func(name, text string, assets AssetOpener) (Page, error)

// Pages hands out every page file of the zip as a page, under the page of the
// folder it sits in, read by read (a Word file as Word).
func (z *ZipArchive) Pages(ctx context.Context, out Output, read PageReader) error {
	var pages []string
	for _, name := range z.names {
		if hasExt(name, pageExtensions...) {
			pages = append(pages, name)
		}
	}
	if len(pages) == 0 {
		return readError{"The zip has no Markdown, HTML, text or Word files in it."}
	}
	if len(pages) > maxZipPages {
		return readError{fmt.Sprintf("The zip holds %d pages; an import makes at most %d docs. Import it in parts, a folder at a time.", len(pages), maxZipPages)}
	}
	prefix := commonFolder(pages)
	assets := z.Assets()
	// The folders pages sit in, so each gets a page.
	pageByDir := map[string]string{}
	for _, p := range pages {
		pageByDir[strings.TrimSuffix(p, path.Ext(p))] = p
	}
	folders := map[string]bool{}
	for _, p := range pages {
		for dir := path.Dir(p); dir != "." && dir != prefix; dir = path.Dir(dir) {
			if _, ok := pageByDir[dir]; !ok {
				folders[dir] = true
			}
		}
	}
	parentOf := func(key string) string {
		dir := path.Dir(key)
		if dir == "." || dir == prefix {
			return ""
		}
		if p, ok := pageByDir[dir]; ok {
			return p
		}
		return dir + "/"
	}
	var folderList []string
	for f := range folders {
		folderList = append(folderList, f)
	}
	sort.Strings(folderList)
	for _, f := range folderList {
		key := f + "/"
		if err := out.Page(Page{Key: key, ParentKey: parentOf(f), Title: path.Base(f), Body: emptyBody()}); err != nil {
			return err
		}
	}
	for i, name := range pages {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pg, err := z.page(name, assets, read)
		if err != nil {
			out.Skip(name, err.Error())
			continue
		}
		pg.ParentKey = parentOf(name)
		if err := out.Page(pg); err != nil {
			return err
		}
		out.Progress(i+1, len(pages))
	}
	return nil
}

// page reads one page file of the zip.
func (z *ZipArchive) page(name string, assets AssetOpener, read PageReader) (Page, error) {
	if hasExt(name, ".docx") {
		data, err := z.Read(name, maxDocxPart)
		if err != nil {
			return Page{}, err
		}
		pg, err := docxPage(name, bytes.NewReader(data), int64(len(data)), z.budget)
		if err != nil {
			return Page{}, fmt.Errorf("not a Word file that can be read")
		}
		// Its images are inside it; the zip's files are not its own.
		return pg, nil
	}
	text, err := z.ReadText(name)
	if err != nil {
		return Page{}, err
	}
	if read != nil {
		return read(name, text, assets)
	}
	return TextPage(name, text, assets), nil
}

// TextPage reads a Markdown, HTML or text file as a page, as its extension
// says, its images opened through assets.
func TextPage(name, text string, assets AssetOpener) Page {
	switch {
	case hasExt(name, ".md", ".markdown"):
		return markdownPage(name, text, assets)
	case hasExt(name, ".html", ".htm"):
		return htmlPage(name, text, assets)
	}
	return textPage(name, text)
}

// emptyBody is the body of a doc made for a folder.
func emptyBody() *contentcodec.Node { return contentcodec.Normalize(nil, nil) }

// zipName is an entry's name as text. A name that isn't marked as UTF-8 and
// isn't UTF-8 is in the zip's own code page, CP437, as Windows' built-in zip
// writes one.
func zipName(f *zip.File) string {
	if f.NonUTF8 && !utf8.ValidString(f.Name) {
		if name, err := charmap.CodePage437.NewDecoder().String(f.Name); err == nil {
			return name
		}
	}
	return f.Name
}

// cleanZipPath is an entry's path, or "" for one that climbs out of the zip.
func cleanZipPath(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if strings.HasPrefix(name, "/") {
		return ""
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return ""
	}
	return clean
}

// hiddenZipPath is an entry nobody meant to import: macOS's resource forks,
// dot-files.
func hiddenZipPath(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || part == "__MACOSX" {
			return true
		}
	}
	return false
}

// commonFolder is the deepest folder every page sits in, when they all sit in
// one (a zip of a folder, or of folders in folders, as Google Takeout wraps
// Drive in Takeout/Drive), so it does not become a page of its own.
func commonFolder(pages []string) string {
	common := path.Dir(pages[0])
	for _, p := range pages[1:] {
		for common != "." && !strings.HasPrefix(p, common+"/") {
			common = path.Dir(common)
		}
	}
	if common == "." {
		return ""
	}
	return common
}

// errUnpackBudget is a file of a zip that would take it past what an import
// unpacks (unpackBudget).
var errUnpackBudget = errors.New("the zip unpacks to more than an import reads")

// unpackBudget is what a zip may still unpack.
type unpackBudget struct {
	left int64
}

func (b *unpackBudget) read(f *zip.File, max int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("could not be read out of the zip")
	}
	defer rc.Close()
	limit := min(max, b.left)
	data, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, fmt.Errorf("could not be read out of the zip")
	}
	if int64(len(data)) > limit {
		if limit == b.left {
			return nil, errUnpackBudget
		}
		return nil, fmt.Errorf("is larger than a page can be (%d MB)", max>>20)
	}
	b.left -= int64(len(data))
	return data, nil
}

// spill unpacks f to a file of its own, within the budget.
func (b *unpackBudget) spill(f *zip.File) (*os.File, int64, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, 0, readError{fmt.Sprintf("%s could not be read out of the zip.", path.Base(f.Name))}
	}
	defer rc.Close()
	tmp, err := os.CreateTemp("", "import-part-*.zip")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(tmp, io.LimitReader(rc, b.left+1))
	switch {
	case err != nil:
		err = readError{fmt.Sprintf("%s could not be read out of the zip.", path.Base(f.Name))}
	case n > b.left:
		err = readError{"The zip unpacks to more than an import reads."}
	}
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, 0, err
	}
	b.left -= n
	return tmp, n, nil
}

// zipAssets opens a zip's files for the pages in it.
type zipAssets struct {
	files  map[string]*zip.File
	budget *unpackBudget
}

// Open opens a file to be read as it is needed, so a caller can turn it away
// by its size without reading it; what is read comes out of the budget, and
// never more than the size the zip says the file has. That size is the zip's
// word, up to 2⁶⁴: one past what the import may still unpack is refused
// unread (as an int64 a size past 2⁶³ is negative, which every "too large"
// check after this would pass).
func (a zipAssets) Open(p string) (io.ReadCloser, int64, error) {
	f := a.files[p]
	if f == nil {
		return nil, 0, ErrNotFound
	}
	if a.budget.left < 0 || f.UncompressedSize64 > uint64(a.budget.left) {
		return nil, 0, errUnpackBudget
	}
	size := int64(f.UncompressedSize64)
	rc, err := f.Open()
	if err != nil {
		return nil, 0, fmt.Errorf("could not be read out of the zip")
	}
	return &budgeted{Reader: io.LimitReader(rc, size), Closer: rc, budget: a.budget}, size, nil
}

// budgeted is a file of the zip as it is read, each byte taken from the zip's
// budget.
type budgeted struct {
	io.Reader
	io.Closer
	budget *unpackBudget
}

func (b *budgeted) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.budget.left -= int64(n)
	if b.budget.left < 0 && err == nil {
		err = errUnpackBudget
	}
	return n, err
}
