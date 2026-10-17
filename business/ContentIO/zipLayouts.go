package business

import (
	"context"
	"fmt"
	"sync"
)

// ZipLayout reads one tool's export zip, where the plain folder reading would
// lose what the tool put in it: Confluence's page tree, Notion's icons and
// databases, Google Takeout's wrapping folders. An adapter registers one from
// its package's init (business/ContentIO/adapters/*):
//
//	func init() { contentIOBusiness.RegisterZipLayout(confluenceLayout{}) }
//
// A zip goes to the first layout that claims it by the names in it, else to
// the plain reading.
type ZipLayout interface {
	// Name names it in the logs and in tests: "notion", "confluence".
	Name() string
	// Claims says whether a zip with these files (ZipArchive.Names) is this
	// tool's export. It reads names only, so asking every layout costs nothing.
	Claims(names []string) bool
	// Read hands the zip's pages to out.
	Read(ctx context.Context, z *ZipArchive, out Output) error
}

var (
	layoutsMu sync.RWMutex
	layouts   []ZipLayout
)

// RegisterZipLayout adds a layout; two with one name panic.
func RegisterZipLayout(l ZipLayout) {
	layoutsMu.Lock()
	defer layoutsMu.Unlock()
	for _, have := range layouts {
		if have.Name() == l.Name() {
			panic(fmt.Sprintf("content io: two zip layouts named %q", l.Name()))
		}
	}
	layouts = append(layouts, l)
}

// layoutFor is the layout that claims a zip with these files, or nil.
func layoutFor(names []string) ZipLayout {
	layoutsMu.RLock()
	defer layoutsMu.RUnlock()
	for _, l := range layouts {
		if l.Claims(names) {
			return l
		}
	}
	return nil
}
