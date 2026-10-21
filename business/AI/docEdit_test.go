package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	adapter "github.com/akashc777/OneCamp/adapter/Doc"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	proposalBusiness "github.com/akashc777/OneCamp/business/Proposal"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	auditModel "github.com/akashc777/OneCamp/models/postgres/AdminAudit"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

// liveDoc is a doc as the Live Doc Writer holds it, in memory, with the
// writer's own rules: a block keeps the id its HTML carries and a new block
// gets one, ops find top-level blocks by id, and a version is kept first.
type liveDoc struct {
	blocks   []contentcodec.Block
	versions map[string]string
	writes   []collab.Options
	n        int
}

func newLiveDoc(html string) *liveDoc {
	return &liveDoc{blocks: blocksOf(html), versions: map[string]string{}}
}

// blocksOf is HTML's top-level blocks, as the content codec reads them.
func blocksOf(html string) []contentcodec.Block {
	blocks, err := contentcodec.Blocks(html, contentcodec.BlockOptions{})
	if err != nil {
		panic(err)
	}
	return blocks
}

func (d *liveDoc) html() string {
	var b strings.Builder
	for _, blk := range d.blocks {
		b.WriteString(blk.HTML)
	}
	return b.String()
}

// parse is HTML as the writer's blocks: an id kept, a missing one given.
func (d *liveDoc) parse(html string) []contentcodec.Block {
	var out []contentcodec.Block
	for _, b := range blocksOf(html) {
		if b.ID == "" {
			d.n++
			b.ID = fmt.Sprintf("new-%d", d.n)
			i := strings.Index(b.HTML, ">")
			b.HTML = b.HTML[:i] + ` data-block-id="` + b.ID + `"` + b.HTML[i:]
		}
		out = append(out, blocksOf(b.HTML)...)
	}
	return out
}

func (d *liveDoc) index(id string) int {
	for i, b := range d.blocks {
		if b.ID == id {
			return i
		}
	}
	return -1
}

func (d *liveDoc) write(at int, removed []string, html string, o collab.Options) (collab.Result, error) {
	r := collab.Result{Persisted: true}
	if o.SnapshotFirst {
		d.n++
		r.VersionID = fmt.Sprintf("v-%d", d.n)
		d.versions[r.VersionID] = d.html()
	}
	kept := d.blocks[:0:0]
	for _, b := range d.blocks {
		gone := false
		for _, id := range removed {
			gone = gone || b.ID == id
		}
		if !gone {
			kept = append(kept, b)
		}
	}
	added := d.parse(html)
	d.blocks = append(append(append([]contentcodec.Block{}, kept[:at]...), added...), kept[at:]...)
	for _, b := range added {
		r.BlockIDs = append(r.BlockIDs, b.ID)
	}
	r.Blocks, r.Removed = len(added), len(removed)
	if o.AgentEdit && o.By.AgentUUID != "" && len(added) > 0 {
		d.n++
		r.EditID = fmt.Sprintf("edit-%d", d.n)
	}
	d.writes = append(d.writes, o)
	return r, nil
}

var errNoBlock = &collab.OpError{Op: "write", Status: http.StatusNotFound, Msg: "no such block"}

func (d *liveDoc) Append(_ context.Context, _, html string, o collab.Options) (collab.Result, error) {
	return d.write(len(d.blocks), nil, html, o)
}

func (d *liveDoc) InsertAfter(_ context.Context, _, id, html string, o collab.Options) (collab.Result, error) {
	i := d.index(id)
	if i < 0 {
		return collab.Result{}, errNoBlock
	}
	return d.write(i+1, nil, html, o)
}

func (d *liveDoc) ReplaceBlocks(_ context.Context, _ string, ids []string, html string, o collab.Options) (collab.Result, error) {
	first := len(d.blocks)
	for _, id := range ids {
		i := d.index(id)
		if i < 0 {
			return collab.Result{}, errNoBlock
		}
		if i < first {
			first = i
		}
	}
	return d.write(first, ids, html, o)
}

func (d *liveDoc) ReplaceAll(context.Context, string, string, collab.Options) (collab.Result, error) {
	return collab.Result{}, errors.New("not used")
}
func (d *liveDoc) Seed(context.Context, string, string, collab.Options) (collab.Result, error) {
	return collab.Result{}, errors.New("not used")
}
func (d *liveDoc) Reauth(context.Context, string, []string) error { return nil }
func (d *liveDoc) ApplyProposal(context.Context, string, string, []string, collab.Options) (collab.Result, error) {
	return collab.Result{}, errors.New("not used")
}

// auditLog is the record, in memory, read back as the store reads it.
type auditLog struct{ rows []*auditModel.AuditEntry }

func (l *auditLog) list(_ context.Context, doc, prefix string, limit int) ([]*auditModel.AuditEntry, error) {
	var out []*auditModel.AuditEntry
	for i := len(l.rows) - 1; i >= 0; i-- {
		r := l.rows[i]
		var m map[string]interface{}
		_ = json.Unmarshal([]byte(*r.Metadata), &m)
		if strings.HasPrefix(r.Action, prefix) && m["doc_uuid"] == doc {
			out = append(out, r)
		}
	}
	return out, nil
}

const (
	editDoc = "5a2b6c3d-7e8f-4a9b-8c0d-1e2f3a4b5c6d"
	sponsor = "22222222-2222-4222-8222-222222222222"
	asker   = "33333333-3333-4333-8333-333333333333"
	agentID = "44444444-4444-4444-8444-444444444444"
)

const plan = `<h1 data-block-id="h1">Plan</h1>` +
	`<h2 data-block-id="risks">Risks</h2><p data-block-id="r1">Vendor delay</p><h3 data-block-id="sub">Minor</h3><p data-block-id="r2">Typos</p>` +
	`<h2 data-block-id="next">Next steps</h2><p data-block-id="n1">Ship it</p>`

// fakeEditWorld swaps in the live doc, the record and the people. who maps a
// person to their level on the doc.
func fakeEditWorld(t *testing.T, html string, who map[string]accessBusiness.Level) (*liveDoc, *auditLog) {
	t.Helper()
	doc := newLiveDoc(html)
	log := &auditLog{}
	savedLive, savedRead, savedRec, savedList, savedRes, savedPerson, savedAgent, savedVer, savedName :=
		collab.Live, collab.Read, recordEntry, listDocAIEntries, resolveDoc, personByUUID, agentNameOf, versionHTML, personNames
	t.Cleanup(func() {
		collab.Live, collab.Read, recordEntry, listDocAIEntries, resolveDoc, personByUUID, agentNameOf, versionHTML, personNames =
			savedLive, savedRead, savedRec, savedList, savedRes, savedPerson, savedAgent, savedVer, savedName
	})
	collab.Live = doc
	collab.Read = func(context.Context, string) (collab.Current, error) { return collab.Current{HTML: doc.html()}, nil }
	at := time.Unix(1_000, 0)
	recordEntry = func(_ context.Context, _ *uuid.UUID, email, kind, action, summary string, meta map[string]interface{}) error {
		b, _ := json.Marshal(meta)
		s := string(b)
		at = at.Add(time.Second)
		log.rows = append(log.rows, &auditModel.AuditEntry{Seq: int64(len(log.rows) + 1), Action: action, Summary: summary, ActorKind: kind, ActorEmail: email, Metadata: &s, CreatedAt: at})
		return nil
	}
	listDocAIEntries = log.list
	resolveDoc = func(_ context.Context, s accessBusiness.Subject, _ string) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{Level: who[s.UserUUID]}, nil
	}
	personByUUID = func(_ context.Context, id string) (*userModels.UserInfo, error) {
		u := &userModels.UserInfo{}
		u.UserDgraphInfo.Uuid, u.UserDgraphInfo.Uid, u.UserDgraphInfo.UserFullName = id, "uid-"+id, "Person "+id[:2]
		u.UserPostgresInfo.EmailID = id[:2] + "@example.com"
		return u, nil
	}
	agentNameOf = func(context.Context, string) string { return "Release Captain" }
	versionHTML = func(_ context.Context, _ accessBusiness.Subject, _, id string) (string, error) {
		v, ok := doc.versions[id]
		if !ok {
			return "", errors.New("gone")
		}
		return v, nil
	}
	personNames = func(_ context.Context, ids []string) map[string]string {
		out := map[string]string{}
		for _, id := range ids {
			out[id] = "Person " + id[:2]
		}
		return out
	}
	return doc, log
}

// agentRun is the context of an agent's run that asker asked for.
func agentRun(runID string) context.Context {
	ctx := ai.WithRunRequester(context.Background(), asker, sponsor)
	ctx = ai.WithSandboxScope(ctx, ai.SandboxScope{AgentID: agentID, RunID: runID})
	return ai.WithAgentBudget(ctx, agentID, 0)
}

func call(tool string, params map[string]string) ai.ProposedAction {
	return ai.ProposedAction{ToolName: tool, Params: params}
}

var editors = map[string]accessBusiness.Level{sponsor: accessBusiness.LevelEdit, asker: accessBusiness.LevelEdit}

func person(id string) *userModels.UserInfo {
	u := &userModels.UserInfo{}
	u.UserDgraphInfo.Uuid, u.UserDgraphInfo.Uid = id, "uid-"+id
	return u
}

// An agent asked to replace the Risks section changes only the blocks under
// that heading; Revert puts back exactly what was there (editor.md P2-2).
func TestReplacingASectionChangesOnlyItAndRevertRestoresExactly(t *testing.T) {
	doc, log := fakeEditWorld(t, plan, editors)
	before := doc.html()
	out, meta, err := executeDocReplaceSection(agentRun("run-1"), call("doc_replace_section", map[string]string{
		"doc_uuid": editDoc, "heading": "## risks:", "content": "- Vendor delay of two weeks\n- Venue not confirmed",
	}), sponsor)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Replaced the “Risks” section (3 block(s) out, 1 in)") || meta["edit_id"] == "" {
		t.Errorf("result: %q %v", out, meta)
	}
	got := doc.html()
	if !strings.HasPrefix(got, `<h1 data-block-id="h1">Plan</h1><h2 data-block-id="risks">Risks</h2><ul`) ||
		!strings.HasSuffix(got, `<h2 data-block-id="next">Next steps</h2><p data-block-id="n1">Ship it</p>`) ||
		strings.Contains(got, "Typos") || !strings.Contains(got, "Venue not confirmed") {
		t.Fatalf("the doc after the edit: %s", got)
	}
	// The record: the agent, the asker, the run, what was written and replaced.
	if len(log.rows) != 1 || log.rows[0].Action != ActionDocAgentEditAllowed || log.rows[0].ActorKind != "agent" {
		t.Fatalf("rows: %+v", log.rows)
	}
	var m agentEditRow
	_ = json.Unmarshal([]byte(*log.rows[0].Metadata), &m)
	if m.AgentID != agentID || m.AgentName != "Release Captain" || m.AskerUUID != asker || m.RunID != "run-1" || m.Op != opReplaceSection ||
		strings.Join(m.ReplacedIDs, ",") != "r1,sub,r2" || len(m.BlockIDs) != 1 || m.VersionID == "" || len(m.Hash) != 64 {
		t.Errorf("row: %+v", m)
	}
	if strings.Contains(*log.rows[0].Metadata, "Vendor") {
		t.Error("the row keeps the doc's text")
	}

	n, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "run-1", "", false)
	if err != nil || n != 1 {
		t.Fatalf("revert: %d %v", n, err)
	}
	if doc.html() != before {
		t.Errorf("revert did not restore exactly:\n got %s\nwant %s", doc.html(), before)
	}
	if last := log.rows[len(log.rows)-1]; last.Action != ActionDocAgentEditRevert || !strings.Contains(*last.Metadata, m.EditID) {
		t.Errorf("the revert was not recorded: %+v", last)
	}
	// Reverted once: there is nothing left to revert.
	if _, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "run-1", "", false); err == nil {
		t.Error("a second revert of the same run went ahead")
	}
}

// Adding to a section puts the content at its end, below its subsections, and
// before the next heading of its level; revert takes it out again.
func TestAddingToASectionGoesAtItsEnd(t *testing.T) {
	doc, _ := fakeEditWorld(t, plan, editors)
	before := doc.html()
	if _, _, err := executeDocInsertAfter(agentRun("run-2"), call("doc_insert_after", map[string]string{
		"doc_uuid": editDoc, "heading": "Risks", "content": "Budget overrun",
	}), sponsor); err != nil {
		t.Fatal(err)
	}
	got := doc.html()
	if !strings.Contains(got, `<p data-block-id="r2">Typos</p><p data-block-id="new-`) || !strings.Contains(got, `Budget overrun</p><h2 data-block-id="next">`) {
		t.Fatalf("the addition went elsewhere: %s", got)
	}
	if n, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "run-2", "", false); err != nil || n != 1 || doc.html() != before {
		t.Errorf("revert: %d %v\n%s", n, err, doc.html())
	}
	// A heading named by its id, as the answer to two headings of one name
	// asks: the end of its section, as by its text.
	if _, _, err := executeDocInsertAfter(agentRun("run-2b"), call("doc_insert_after", map[string]string{
		"doc_uuid": editDoc, "block_id": "risks", "content": "Scope creep",
	}), sponsor); err != nil || !strings.Contains(doc.html(), `Typos</p><p data-block-id="new-`) || !strings.Contains(doc.html(), `Scope creep</p><h2 data-block-id="next">`) {
		t.Fatalf("by the heading's id: %v %s", err, doc.html())
	}
	// After a block named by id, even one inside a list: after its top-level block.
	if _, _, err := executeDocInsertAfter(agentRun("run-3"), call("doc_insert_after", map[string]string{
		"doc_uuid": editDoc, "block_id": "r1", "content": "Right after the first risk",
	}), sponsor); err != nil || !strings.Contains(doc.html(), `Vendor delay</p><p data-block-id="new-`) {
		t.Fatalf("after a block: %v %s", err, doc.html())
	}
}

func TestAppendingThenRevertingLeavesTheDocAsItWas(t *testing.T) {
	doc, _ := fakeEditWorld(t, plan, editors)
	before := doc.html()
	out, _, err := executeAppendToDoc(agentRun("run-4"), call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": "## Rollback\n1. Revert the deploy\n2. Restore the snapshot"}), sponsor)
	if err != nil || !strings.Contains(out, "Added 2 block(s) to the end") {
		t.Fatalf("%q %v", out, err)
	}
	if !strings.Contains(doc.html(), "<ol") {
		t.Errorf("the Markdown didn't arrive as structure: %s", doc.html())
	}
	if n, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "", "", false); err == nil || n != 0 {
		t.Error("a revert naming nothing went ahead")
	}
	edits, err := ListAgentEdits(context.Background(), person(asker), editDoc)
	if err != nil || len(edits) != 1 || !edits[0].CanRevert || edits[0].AgentName != "Release Captain" || edits[0].AskerName != "Person 33" || edits[0].Op != opAppend {
		t.Fatalf("the margin chip's edit: %+v %v", edits, err)
	}
	if n, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "", edits[0].EditID, false); err != nil || n != 1 || doc.html() != before {
		t.Errorf("revert by edit: %d %v", n, err)
	}
	if edits, _ := ListAgentEdits(context.Background(), person(asker), editDoc); !edits[0].Reverted || edits[0].CanRevert {
		t.Errorf("a reverted edit: %+v", edits[0])
	}
}

// When someone has edited what the agent wrote since, Revert asks first, and
// goes ahead only when told to.
func TestRevertAsksWhenPeopleEditedTheAgentsText(t *testing.T) {
	doc, _ := fakeEditWorld(t, plan, editors)
	before := doc.html()
	if _, _, err := executeDocReplaceSection(agentRun("run-5"), call("doc_replace_section", map[string]string{"doc_uuid": editDoc, "heading": "Risks", "content": "One risk"}), sponsor); err != nil {
		t.Fatal(err)
	}
	for i, b := range doc.blocks {
		if strings.Contains(b.HTML, "One risk") {
			doc.blocks[i] = blocksOf(strings.Replace(b.HTML, "One risk", "One risk, and a person's note", 1))[0]
		}
	}
	var since *EditedSinceError
	if _, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "run-5", "", false); !errors.As(err, &since) {
		t.Fatalf("got %v, want a question first", err)
	}
	if _, err := RevertAgentEdits(context.Background(), person(asker), editDoc, "run-5", "", true); err != nil || doc.html() != before {
		t.Errorf("forced: %v\n%s", err, doc.html())
	}
}

// Every agent edit carries the agent and the person who asked it, is marked as
// the agent's and keeps the doc first; the assistant acting for a person
// directly writes as that person.
func TestAnAgentEditCarriesTheAgentAndTheAsker(t *testing.T) {
	doc, _ := fakeEditWorld(t, plan, editors)
	if _, _, err := executeAppendToDoc(agentRun("run-6"), call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": "x"}), sponsor); err != nil {
		t.Fatal(err)
	}
	o := doc.writes[0]
	if o.By.UserUUID != asker || o.By.AgentUUID != agentID || !o.AgentEdit || o.RunID != "run-6" || !o.SnapshotFirst {
		t.Errorf("an agent's write: %+v", o)
	}
	if _, _, err := executeAppendToDoc(context.Background(), call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": "y"}), sponsor); err != nil {
		t.Fatal(err)
	}
	if o := doc.writes[1]; o.By.UserUUID != sponsor || o.By.AgentUUID != "" || o.AgentEdit || o.SnapshotFirst {
		t.Errorf("the assistant's write: %+v", o)
	}
	// An agent's proposed write that a person approved runs as that person, and
	// is still the agent's edit: marked, kept first, revertable.
	approved := ai.WithApprovedAgentProposal(context.Background(), agentID)
	if _, _, err := executeAppendToDoc(approved, call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": "z"}), asker); err != nil {
		t.Fatal(err)
	}
	if o := doc.writes[2]; o.By.UserUUID != asker || o.By.AgentUUID != agentID || !o.AgentEdit || !o.SnapshotFirst {
		t.Errorf("an approved agent proposal's write: %+v", o)
	}
}

// An agent whose name can't be read is still named in the record's line.
func TestAnAgentWithoutANameIsStillNamed(t *testing.T) {
	_, log := fakeEditWorld(t, plan, editors)
	agentNameOf = func(context.Context, string) string { return "" }
	if _, _, err := executeAppendToDoc(agentRun("run-9"), call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": "x"}), sponsor); err != nil {
		t.Fatal(err)
	}
	if got := log.rows[0].Summary; got != "An agent added to the end of a doc" {
		t.Errorf("summary: %q", got)
	}
}

// An agent asked by someone who can only read the doc writes nothing, whatever
// its sponsor may do, and the refusal is recorded.
func TestAnAgentIsRefusedWhatItsAskerCantDo(t *testing.T) {
	doc, log := fakeEditWorld(t, plan, map[string]accessBusiness.Level{sponsor: accessBusiness.LevelFull, asker: accessBusiness.LevelView})
	before := doc.html()
	for _, tool := range []string{"append_to_doc", "doc_insert_after", "doc_replace_section"} {
		exec := map[string]func(context.Context, ai.ProposedAction, string) (string, map[string]string, error){
			"append_to_doc": executeAppendToDoc, "doc_insert_after": executeDocInsertAfter, "doc_replace_section": executeDocReplaceSection,
		}[tool]
		_, _, err := exec(agentRun("run-7"), call(tool, map[string]string{"doc_uuid": editDoc, "heading": "Risks", "content": "x"}), sponsor)
		if err == nil || !strings.Contains(err.Error(), "can be read but not edited") {
			t.Errorf("%s: %v", tool, err)
		}
	}
	if doc.html() != before || len(doc.writes) != 0 {
		t.Error("a refused edit changed the doc")
	}
	if len(log.rows) != 3 {
		t.Fatalf("rows: %d", len(log.rows))
	}
	for _, r := range log.rows {
		if r.Action != ActionDocAgentEditRefused || !strings.Contains(*r.Metadata, editDoc) || !strings.Contains(*r.Metadata, `"agent_name":"Release Captain"`) {
			t.Errorf("refusal row: %s %s", r.Action, *r.Metadata)
		}
	}
	// A doc nobody can open is answered as one that doesn't exist, in the one
	// sentence for both (helpers.DocNotFound).
	fakeEditWorld(t, plan, map[string]accessBusiness.Level{})
	if _, _, err := executeAppendToDoc(agentRun("run-8"), call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": "x"}), sponsor); err == nil || err.Error() != helpers.DocNotFound {
		t.Errorf("an outsider's doc: %v, want %q", err, helpers.DocNotFound)
	}
}

func TestAHeadingThatIsNotOneOrIsTwoIsSaidPlainly(t *testing.T) {
	fakeEditWorld(t, plan+`<h2 data-block-id="risks2">Risks</h2><p data-block-id="x">Again</p>`, editors)
	_, _, err := executeDocReplaceSection(agentRun("r"), call("doc_replace_section", map[string]string{"doc_uuid": editDoc, "heading": "Risks", "content": "x"}), sponsor)
	if err == nil || !strings.Contains(err.Error(), "there are 2 headings called “Risks”") || !strings.Contains(err.Error(), "risks, risks2") {
		t.Errorf("two headings: %v", err)
	}
	_, _, err = executeDocReplaceSection(agentRun("r"), call("doc_replace_section", map[string]string{"doc_uuid": editDoc, "heading": "Budget", "content": "x"}), sponsor)
	if err == nil || !strings.Contains(err.Error(), "no heading called “Budget”") || !strings.Contains(err.Error(), "“Next steps”") {
		t.Errorf("no such heading: %v", err)
	}
	// Named by its id instead, the second one is the one changed.
	if _, _, err := executeDocReplaceSection(agentRun("r"), call("doc_replace_section", map[string]string{"doc_uuid": editDoc, "heading": "Risks", "block_id": "risks2", "content": "Only here"}), sponsor); err != nil {
		t.Fatal(err)
	}
}

// A suggestion is a proposal for the doc's people to review, never a write.
func TestASuggestionIsAProposalNotAnEdit(t *testing.T) {
	doc, log := fakeEditWorld(t, plan, map[string]accessBusiness.Level{sponsor: accessBusiness.LevelEdit, asker: accessBusiness.LevelComment})
	saved := createProposal
	t.Cleanup(func() { createProposal = saved })
	var got proposalBusiness.Proposal
	var by accessBusiness.Subject
	createProposal = func(_ context.Context, who accessBusiness.Subject, p proposalBusiness.Proposal) (proposalBusiness.Proposal, error) {
		got, by = p, who
		p.ID, p.Hunks = "prop-1", []proposalBusiness.Hunk{{ID: "h1"}, {ID: "h2"}}
		return p, nil
	}
	out, meta, err := executeDocSuggest(agentRun("run-9"), call("doc_suggest", map[string]string{"doc_uuid": editDoc, "heading": "Risks", "content": "- Fewer risks", "note": "Tightened the risks"}), sponsor)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.writes) != 0 {
		t.Error("a suggestion wrote into the doc")
	}
	if strings.Join(got.Draft.Target.BlockIDs, ",") != "r1,sub,r2" || !strings.Contains(got.Draft.HTML, "Fewer risks") || got.RunID != "run-9" || got.Note != "Tightened the risks" {
		t.Errorf("proposal: %+v %+v", got, got.Draft)
	}
	if by.UserUUID != asker || by.AgentUUID != agentID {
		t.Errorf("proposed as %+v", by)
	}
	if meta["proposal_id"] != "prop-1" || !strings.Contains(out, "2 change(s)") {
		t.Errorf("result: %q %v", out, meta)
	}
	if len(log.rows) != 1 || !strings.Contains(*log.rows[0].Metadata, `"op":"suggest"`) {
		t.Errorf("record: %+v", log.rows)
	}
	// An empty section's suggestion goes after its heading; insert_after goes at its end.
	_, _, _ = executeDocSuggest(agentRun("r"), call("doc_suggest", map[string]string{"doc_uuid": editDoc, "heading": "Risks", "mode": "insert_after", "content": "x"}), sponsor)
	if got.Draft.Target.AfterBlockID != "r2" {
		t.Errorf("insert_after: %+v", got.Draft.Target)
	}
}

// create_doc answers the new doc's id, so an agent can go on writing in it, and
// its body is seeded as the person and the agent.
func TestCreateDocAnswersItsIDAndSeedsAsTheAgent(t *testing.T) {
	savedCreate, savedSeed, savedUser := createDocAs, seedDocBody, createDocUser
	t.Cleanup(func() { createDocAs, seedDocBody, createDocUser = savedCreate, savedSeed, savedUser })
	createDocUser = func(context.Context, string) (*dgraphStruct.DgraphUser, error) {
		return &dgraphStruct.DgraphUser{Uuid: sponsor, Uid: "0x2"}, nil
	}
	createDocAs = func(_ context.Context, _ *dgraphStruct.DgraphUser, in *adapter.InputCreateDoc, _ ...string) (*dgraphStruct.DgraphDoc, error) {
		return &dgraphStruct.DgraphDoc{Uuid: editDoc, Title: in.DocTitle}, nil
	}
	var seeded string
	var by collab.By
	seedDocBody = func(_ context.Context, _, html string, b collab.By) (collab.Result, error) {
		seeded, by = html, b
		return collab.Result{}, nil
	}
	out, meta, err := executeCreateDoc(agentRun("run-10"), call("create_doc", map[string]string{"title": "Launch plan", "body": "## Goals\n- Ship"}), sponsor)
	if err != nil || meta["doc_uuid"] != editDoc || !strings.Contains(out, editDoc) {
		t.Fatalf("%q %v %v", out, meta, err)
	}
	if !strings.Contains(seeded, "<h2") || !strings.Contains(seeded, "<ul") || by.UserUUID != asker || by.AgentUUID != agentID {
		t.Errorf("seeded %q as %+v", seeded, by)
	}
}

// The agent edits a doc lists come newest first, and a person who can only
// read sees them without Revert.
func TestTheMarginChipsListTheAgentsEditsNewestFirst(t *testing.T) {
	_, _ = fakeEditWorld(t, plan, map[string]accessBusiness.Level{sponsor: accessBusiness.LevelEdit, asker: accessBusiness.LevelEdit, "55555555-5555-4555-8555-555555555555": accessBusiness.LevelView})
	for _, run := range []string{"a", "b"} {
		if _, _, err := executeAppendToDoc(agentRun(run), call("append_to_doc", map[string]string{"doc_uuid": editDoc, "content": run}), sponsor); err != nil {
			t.Fatal(err)
		}
	}
	edits, err := ListAgentEdits(context.Background(), person("55555555-5555-4555-8555-555555555555"), editDoc)
	if err != nil || len(edits) != 2 {
		t.Fatalf("%+v %v", edits, err)
	}
	if !sort.SliceIsSorted(edits, func(i, j int) bool { return edits[i].At.After(edits[j].At) }) || edits[0].RunID != "b" {
		t.Errorf("not newest first: %+v", edits)
	}
	if edits[0].CanRevert {
		t.Error("a reader may revert")
	}
}
