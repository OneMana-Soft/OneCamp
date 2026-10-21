package dgfixture

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// fakeSink gives every blank node of a batch a uid of its own, as Dgraph
// does, and keeps what it was sent.
type fakeSink struct {
	next    int
	sum     [32]byte
	batches [][]byte
	keep    bool
	quads   []string // every N-Quad, when keep
}

var blank = regexp.MustCompile(`_:([A-Za-z0-9]+)`)

func (s *fakeSink) Mutate(_ context.Context, nquads []byte) (map[string]string, error) {
	h := sha256.New()
	h.Write(s.sum[:])
	h.Write(nquads)
	copy(s.sum[:], h.Sum(nil))
	out := map[string]string{}
	for _, m := range blank.FindAllSubmatch(nquads, -1) {
		k := string(m[1])
		if _, ok := out[k]; !ok {
			s.next++
			out[k] = fmt.Sprintf("0x%x", s.next)
		}
	}
	if s.keep {
		sc := bufio.NewScanner(bytes.NewReader(nquads))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			s.quads = append(s.quads, sc.Text())
		}
	}
	s.batches = append(s.batches, nil)
	return out, nil
}

// tiny is a tier with every shape, small enough to read whole.
var tiny = Tier{
	Name:   "tiny",
	People: 40, Bots: 2, External: 3, DeletedPeople: 2,
	Teams: 3, Projects: 6,
	Channels: 6, ViewerChannels: 4, ChannelMembersMin: 3, ChannelMembersMax: 10,
	Posts: 900, SuperPosterPosts: 300,
	DMs: 30, ViewerDMs: 8, Groups: 5, ViewerGroups: 2,
	Chats: 500, BusiestDMChats: 120,
	Docs: 300, DocDepth: 6, DocParentChildren: 40, Backlinks: 100,
	Boards: 6, Tasks: 400, BoardTasks: 120, ChainLength: 10, MemoryItems: 20,
	DocPosts: 60, RefPosts: 60,
}

func generate(t *testing.T, tier Tier, seed uint64, keep bool) (*Fixture, *fakeSink) {
	t.Helper()
	s := &fakeSink{keep: keep}
	f, err := Generate(context.Background(), tier, seed, s)
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

// The same seed writes the same workspace, byte for byte, and another seed
// another one: a budget measured today is measured on what tomorrow's run
// builds.
func TestTheSameSeedWritesTheSameWorkspace(t *testing.T) {
	_, a := generate(t, tiny, 7, false)
	_, b := generate(t, tiny, 7, false)
	_, c := generate(t, tiny, 8, false)
	if a.sum != b.sum {
		t.Error("two runs with one seed wrote different N-Quads")
	}
	if a.sum == c.sum {
		t.Error("two seeds wrote the same N-Quads")
	}
	f1, _ := generate(t, tiny, 7, false)
	f2, _ := generate(t, tiny, 7, false)
	if f1.Viewer.UUID != f2.Viewer.UUID || f1.ChainHead != f2.ChainHead || f1.BusiestDM != f2.BusiestDM {
		t.Error("the handles differ between two runs with one seed")
	}
}

// Every tier writes what it says, super-nodes included.
func TestEachTierWritesItsSizeAndItsSuperNodes(t *testing.T) {
	for _, tier := range []Tier{tiny, Small} {
		t.Run(tier.Name, func(t *testing.T) {
			f, s := generate(t, tier, DefaultSeed, tier.Name == "tiny")
			want := map[string]int{
				"people": tier.People, "teams": tier.Teams, "projects": tier.Projects, "channels": tier.Channels,
				"posts": tier.Posts, "superposts": tier.SuperPosterPosts, "groups": tier.Groups, "chats": tier.Chats,
				"docs": tier.Docs, "boards": tier.Boards, "tasks": tier.Tasks, "memory": tier.MemoryItems, "deletedShares": 1, "docPosts": tier.DocPosts, "refPosts": tier.RefPosts,
			}
			for kind, n := range want {
				if f.Counts[kind] != n {
					t.Errorf("%s: %d, want %d", kind, f.Counts[kind], n)
				}
			}
			if f.Counts["dms"] < tier.ViewerDMs || f.Counts["dms"] > tier.DMs {
				t.Errorf("dms: %d, want %d..%d", f.Counts["dms"], tier.ViewerDMs, tier.DMs)
			}
			for _, kind := range []string{"comments", "reactions", "mentions", "dependencies", "docsSharedWithViewer"} {
				if f.Counts[kind] == 0 {
					t.Errorf("no %s", kind)
				}
			}
			if f.Quads == 0 || f.Batches == 0 || len(s.batches) != f.Batches {
				t.Errorf("%d N-Quads in %d batches (%d sent)", f.Quads, f.Batches, len(s.batches))
			}
			for _, h := range []string{f.General, f.ViewerChannel, f.BusiestDM, f.ViewerGroup, f.DocParent, f.DeepDoc, f.BacklinkedDoc, f.SpaceProject, f.BoardProject, f.ChainHead} {
				if h == "" {
					t.Errorf("a handle is empty: %+v", f)
				}
			}
			if f.ViewerUID() == "" || f.UID("ch0") == "" || f.UID("doc0") == "" {
				t.Error("kept uids missing")
			}
			if tier.Name != "tiny" {
				return
			}
			count := func(pred, obj string) int {
				n := 0
				for _, q := range s.quads {
					if strings.Contains(q, "<"+pred+">") && (obj == "" || strings.Contains(q, "> "+obj+" ")) {
						n++
					}
				}
				return n
			}
			general := f.UID("ch0")
			members := 0
			for _, q := range s.quads {
				if strings.HasPrefix(q, "_:ch0 <ch_members>") || strings.HasPrefix(q, "<"+general+"> <ch_members>") {
					members++
				}
			}
			if members != tier.People {
				t.Errorf("#general has %d members, want every person (%d)", members, tier.People)
			}
			if n := count("doc_mentions", "_:doc0") + count("doc_mentions", "<"+f.UID("doc0")+">"); n != tier.Backlinks {
				t.Errorf("the backlinked doc has %d backlinks, want %d", n, tier.Backlinks)
			}
			if n := count("doc_parent", "_:doc1") + count("doc_parent", "<"+f.UID("doc1")+">"); n != tier.DocParentChildren {
				t.Errorf("the wide page has %d children, want %d", n, tier.DocParentChildren)
			}
			if n := count("task_blocked_by", ""); n < tier.ChainLength*3-1 {
				t.Errorf("%d dependencies, want the chain's %d at least", n, tier.ChainLength*3-1)
			}
			live, deleted := count("post_deleted_at", `"`+zeroTime+`"`), 0
			for _, q := range s.quads {
				if strings.Contains(q, "<post_deleted_at>") && !strings.Contains(q, zeroTime) {
					deleted++
				}
			}
			if live+deleted != tier.Posts+tier.DocPosts+tier.RefPosts || deleted == 0 || live == 0 {
				t.Errorf("posts: %d live (zero time) and %d deleted (a real time), want %d in all, both kinds", live, deleted, tier.Posts+tier.DocPosts+tier.RefPosts)
			}
			if n := count("refers_to", "_:doc0") + count("refers_to", "<"+f.UID("doc0")+">"); n != min(referringPosts, tier.RefPosts) {
				t.Errorf("the backlinked doc is linked from %d posts, want %d", n, min(referringPosts, tier.RefPosts))
			}
			if n := count("post_doc", "_:doc0") + count("post_doc", "<"+f.UID("doc0")+">"); n != sharedDocPosts {
				t.Errorf("the backlinked doc is shared in %d posts, want %d", n, sharedDocPosts)
			}
		})
	}
}

// Every predicate the fixture writes is one the server's schema declares
// (initializers/dgraphInit createSchema): a misspelt one would be a new,
// unindexed predicate that no production read asks for, and the reads
// measured on it would be measured on nothing.
func TestEveryPredicateWrittenIsInTheSchema(t *testing.T) {
	src, err := os.ReadFile("../../../initializers/dgraphInit/connectDgraphDB.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{"dgraph.type": true}
	for _, m := range regexp.MustCompile(`(?m)^\s*([a-z_]+)\s*:\s*\[?(?:string|uid|int|float|bool|dateTime)\]?\s`).FindAllSubmatch(src, -1) {
		declared[string(m[1])] = true
	}
	if len(declared) < 200 {
		t.Fatalf("read %d predicates from the schema: this check stopped reading it", len(declared))
	}
	_, s := generate(t, tiny, 1, true)
	pred := regexp.MustCompile(`^\S+ <([^>]+)>`)
	seen := map[string]bool{}
	for _, q := range s.quads {
		m := pred.FindStringSubmatch(q)
		if m == nil {
			t.Fatalf("not an N-Quad: %s", q)
		}
		seen[m[1]] = true
	}
	for p := range seen {
		if !declared[p] {
			t.Errorf("the fixture writes %s, which the schema doesn't declare", p)
		}
	}
}

// A node pointed at from a batch it isn't in, without its uid kept, would be
// a new empty node: the writer refuses to write that.
func TestAPointerToANodeNeverWrittenIsRefused(t *testing.T) {
	w := newWriter(context.Background(), &fakeSink{})
	defer func() {
		if recover() == nil {
			t.Error("a pointer to a node never written was written")
		}
	}()
	s := w.node("post1", "Post")
	w.edge(s, "post_channel", w.ref("ch9"))
}

func TestTierFromEnv(t *testing.T) {
	for v, want := range map[string]string{"": "small", "small": "small", "scale": "scale"} {
		t.Setenv(TierEnv, v)
		got, err := TierFromEnv()
		if err != nil || got.Name != want {
			t.Errorf("%s=%q: %q, %v", TierEnv, v, got.Name, err)
		}
	}
	t.Setenv(TierEnv, "huge")
	if _, err := TierFromEnv(); err == nil {
		t.Error("an unknown tier was taken")
	}
	for _, tier := range []Tier{Small, Scale, tiny} {
		if err := tier.check(); err != nil {
			t.Error(err)
		}
	}
	bad := tiny
	bad.Backlinks = bad.Docs
	if bad.check() == nil {
		t.Error("a tier with more backlinks than docs was taken")
	}
}
