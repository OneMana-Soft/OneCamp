package businness

// The mentions save hook: after a save of a doc's body, who and what it
// mentions.
//
// PEOPLE. An @mention in the body (<span data-type="mention"
// data-id="<user uuid>@<graph uid>">) tells that person once: the doc keeps who
// its body mentions (resource_mentions), and a save emits doc.mentioned only
// for the people it newly mentions. A repeated save, or a mention moved around,
// tells nobody more; one removed and put back the same day doesn't either (the
// notice is keyed by doc, person and day). Someone editing the doc in that save
// isn't told of a mention of themselves. Whether the person may read the doc,
// and asking the author to share it if not, is the bus's (business/Follow).
//
// Some saves tell nobody, though they keep who the body mentions:
//   - a save that holds nobody's edit (the collaboration service converting a
//     doc as it loads it): nobody mentioned anyone;
//   - a doc's first keeping, when the doc was made before docs' mentions began
//     to be kept (migration 344). resource_mentions started empty, so without
//     this an upgrade's first save of each existing doc told everyone its body
//     already mentioned, by push and by email. A doc made since is kept from
//     its first save, and that save tells as any other.
//
// DOCS. A link to another doc (a +doc chip, data-id="doc:<uuid>", or a link to
// /app/doc/<uuid>) is kept as a doc_mentions edge, the backlinks' source.
// Only an edge that changed is written.

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"time"

	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	domain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
	"golang.org/x/net/html"
)

// Seams for tests: where mentions are kept, and a doc's links.
var (
	syncMentions = func(ctx context.Context, docUUID string, people []string) (followModels.MentionSync, error) {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return followModels.MentionSync{}, nil
		}
		return followModels.SyncMentions(ctx, "doc", docUUID, people)
	}
	readDocLinks  = domain.GetDocReferences
	writeDocLinks = domain.SetDocReferences
)

func init() {
	SaveHooks.Register("doc.mentions", mentionsSaved)
}

// lastRefs is what each doc's last save mentioned and linked to, as this
// instance saw it: a save that changes neither (most saves, made while someone
// types) touches no store at all (I15). Another instance, or a restart, just
// reads and finds nothing changed. Bounded: past refsKept docs it starts over.
var lastRefs = struct {
	sync.Mutex
	seen map[string]string
}{seen: map[string]string{}}

const refsKept = 20000

// unchangedRefs says whether a save mentions and links to what the doc's last
// save did, and remembers this one.
func unchangedRefs(docUUID string, people, docs []string) bool {
	key := strings.Join(people, ",") + "|" + strings.Join(docs, ",")
	lastRefs.Lock()
	defer lastRefs.Unlock()
	if prev, ok := lastRefs.seen[docUUID]; ok && prev == key {
		return true
	}
	if len(lastRefs.seen) >= refsKept {
		lastRefs.seen = map[string]string{}
	}
	lastRefs.seen[docUUID] = key
	return false
}

// forgetRefs drops what a doc's last save was remembered to hold, after a
// save whose references couldn't all be kept: the next save tries again.
func forgetRefs(docUUID string) {
	lastRefs.Lock()
	delete(lastRefs.seen, docUUID)
	lastRefs.Unlock()
}

// mentionsSaved is the hook.
func mentionsSaved(ctx context.Context, c Change) {
	people, docs := bodyReferences(c.Body)
	if unchangedRefs(c.DocUUID, people, docs) {
		return
	}

	// The doc first: its links, and when it was made, for a first keeping.
	doc, derr := readDocLinks(ctx, c.DocUUID)
	kept, err := syncMentions(ctx, c.DocUUID, people)
	if err != nil {
		forgetRefs(c.DocUUID)
		helpers.LogErrorWithContext(ctx, "business/mentionsSaved doc %s: who it mentions couldn't be kept, so nobody is told this time: %+v", c.DocUUID, err)
	}
	if tell := whoToTell(c, kept, doc); len(tell) > 0 {
		actor, data := saveActor(c.Contributors)
		resourceEventBusiness.Bus.Emit(ctx, resourceEventBusiness.Event{
			Kind: resourceEventBusiness.DocMentioned, ResourceKind: "doc", ResourceUUID: c.DocUUID,
			Actor: actor, At: c.SavedAt, Subjects: tell, Data: data,
		})
	}

	if derr != nil || doc == nil {
		forgetRefs(c.DocUUID)
		return
	}
	if _, err := writeDocLinks(ctx, doc, docs); err != nil {
		forgetRefs(c.DocUUID)
		helpers.LogErrorWithContext(ctx, "business/mentionsSaved doc %s: its links to other docs weren't kept: %+v", c.DocUUID, err)
	}
}

// whoToTell is who a save's mentions tell: the people it newly mentions, but
// nobody whose edit it holds. Nobody at all for a save holding nobody's edit,
// or for a doc's first keeping when the doc was made before docs' mentions
// were kept, or isn't known to have been made since (the file comment). Pure.
func whoToTell(c Change, kept followModels.MentionSync, doc *dgraphStruct.DgraphDoc) []string {
	if len(c.Contributors) == 0 && len(c.Agents) == 0 {
		return nil
	}
	if kept.First && !madeSince(doc, kept.Since) {
		return nil
	}
	editing := map[string]bool{}
	for _, id := range c.Contributors {
		editing[id] = true
	}
	var tell []string
	for _, id := range kept.Added {
		if !editing[id] {
			tell = append(tell, id)
		}
	}
	return tell
}

// madeSince says whether doc was made at or after since, when docs' mentions
// began to be kept; false when either isn't known. Pure.
func madeSince(doc *dgraphStruct.DgraphDoc, since time.Time) bool {
	return doc != nil && doc.CreatedAt != nil && !since.IsZero() && !doc.CreatedAt.Before(since)
}

// bodyReferences is who a body @mentions (user uuids) and which docs it links
// to (doc uuids), each once, in the order they appear. Pure.
func bodyReferences(body string) (people, docs []string) {
	seenPerson, seenDoc := map[string]bool{}, map[string]bool{}
	addDoc := func(id string) {
		if u, err := uuid.Parse(id); err == nil && !seenDoc[u.String()] {
			seenDoc[u.String()] = true
			docs = append(docs, u.String())
		}
	}
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return people, docs
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		name, hasAttr := z.TagName()
		if !hasAttr {
			continue
		}
		attrs := map[string]string{}
		for {
			k, v, more := z.TagAttr()
			attrs[string(k)] = string(v)
			if !more {
				break
			}
		}
		switch string(name) {
		case "span":
			switch attrs["data-type"] {
			case "mention":
				// "<user uuid>@<graph uid>"; older chips carry the uuid alone.
				id, _, _ := strings.Cut(attrs["data-id"], "@")
				if u, err := uuid.Parse(id); err == nil && !seenPerson[u.String()] {
					seenPerson[u.String()] = true
					people = append(people, u.String())
				}
			case "entityMention":
				if id, ok := strings.CutPrefix(attrs["data-id"], "doc:"); ok {
					addDoc(id)
				}
			}
		case "a":
			if id, ok := docLink(attrs["href"]); ok {
				addDoc(id)
			}
		}
	}
}

// docLink is the doc a link opens, for a link to /app/doc/<uuid> on this app
// or any (a pasted link keeps its host). Pure.
func docLink(href string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(href))
	if err != nil {
		return "", false
	}
	rest, ok := strings.CutPrefix(u.Path, "/app/doc/")
	if !ok {
		return "", false
	}
	id, _, _ := strings.Cut(rest, "/")
	return id, id != ""
}
