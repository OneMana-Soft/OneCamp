package business

// The titles a reader may see: one batch resolver for everything that names a
// thing to someone without showing them what they can't open (live chips,
// links in table cells, feeds). Each kind of thing registers its reader once,
// from the package that reads that kind (RegisterTitleReader), and every
// caller asks ReadableTitles, so a title is decided the same way wherever it
// is shown.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ItemRef names one thing: its kind ("doc", "task", "project", "person") and
// its id.
type ItemRef struct{ Kind, ID string }

// TitleReader names, of ids (each once, in lower case), the things of its
// kind that reader may open now: each one's title, by id. One it leaves out
// stays hidden.
type TitleReader func(ctx context.Context, ids []string, reader Subject) (map[string]string, error)

var (
	titleReadersMu sync.RWMutex
	titleReaders   = map[string]TitleReader{}
)

// RegisterTitleReader adds the reader for a kind, from its package's init. A
// kind registered twice panics: two packages claimed it.
func RegisterTitleReader(kind string, read TitleReader) {
	titleReadersMu.Lock()
	defer titleReadersMu.Unlock()
	if kind == "" || read == nil {
		panic("access: a title reader needs a kind and a reader")
	}
	if _, dup := titleReaders[kind]; dup {
		panic(fmt.Sprintf("access: the title reader for %q is registered twice", kind))
	}
	titleReaders[kind] = read
}

// titleBatch bounds the ids one read takes.
const titleBatch = 100

// ReadableTitles names each of refs that every one of readers may open now
// (an agent asked by someone reads only what both of them may), keyed by the
// ref as asked. A reader is named as the Access Resolver names them, by graph
// uid and uuid. It fails closed: with no readers, or a reader who is no one
// (no uuid: a workspace-wide doc is open to anyone in the workspace, which no
// one isn't), nothing is named; a kind nobody registered is never named; and
// an error from any read is the whole answer, with no titles.
func ReadableTitles(ctx context.Context, readers []Subject, refs []ItemRef) (map[ItemRef]string, error) {
	out := map[ItemRef]string{}
	if len(readers) == 0 {
		return out, nil
	}
	for _, who := range readers {
		if who.UserUUID == "" {
			return out, nil
		}
	}
	byKind := map[string][]string{}
	asked := map[ItemRef]bool{}
	for _, r := range refs {
		key := ItemRef{Kind: r.Kind, ID: strings.ToLower(r.ID)}
		if !asked[key] {
			asked[key] = true
			byKind[r.Kind] = append(byKind[r.Kind], key.ID)
		}
	}
	kinds := make([]string, 0, len(byKind))
	for kind := range byKind {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	named := map[ItemRef]string{}
	for _, kind := range kinds {
		titleReadersMu.RLock()
		read := titleReaders[kind]
		titleReadersMu.RUnlock()
		if read == nil {
			continue
		}
		ids := byKind[kind]
		for from := 0; from < len(ids); from += titleBatch {
			got, err := titlesForAll(ctx, read, ids[from:min(from+titleBatch, len(ids))], readers)
			if err != nil {
				return nil, err
			}
			for id, title := range got {
				named[ItemRef{Kind: kind, ID: id}] = title
			}
		}
	}
	for _, r := range refs {
		if title, ok := named[ItemRef{Kind: r.Kind, ID: strings.ToLower(r.ID)}]; ok {
			out[r] = title
		}
	}
	return out, nil
}

// titlesForAll is the titles of ids that every reader may open, as the first
// reader's read names them, by lower-case id.
func titlesForAll(ctx context.Context, read TitleReader, ids []string, readers []Subject) (map[string]string, error) {
	var common map[string]string
	for i, who := range readers {
		got, err := read(ctx, ids, who)
		if err != nil {
			return nil, err
		}
		mine := make(map[string]string, len(got))
		for id, title := range got {
			mine[strings.ToLower(id)] = title
		}
		if i == 0 {
			common = mine
			continue
		}
		for id := range common {
			if _, both := mine[id]; !both {
				delete(common, id)
			}
		}
	}
	return common, nil
}
