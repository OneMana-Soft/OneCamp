package business

// Agents editing docs precisely (docs programme B9, editor.md P2-2): add to the
// end of a doc, add after a heading's section or a block, replace what is under
// a heading, or suggest a change for the doc's people to review, and Revert.
//
// LIVE AND ATTRIBUTED. Every write goes through the Live Doc Writer (A4,
// services/collab): one Yjs transaction on the live copy, so people with the
// doc open see it arrive and nothing they type is lost. It is credited to the
// person the agent acts for and to the agent (By{UserUUID: asker, AgentUUID:
// agent}), the written range is marked as the agent's (the doc's agentEdits
// map, which the web app draws as "Release Captain · 2m · Revert"), and the doc
// as it was is kept first as an agent_run version (A4's Snapshot Service).
//
// WHO MAY. The person the tool runs as and, when someone else asked for the
// run, the asker too (business/AIAgent agentRequester.go) must be able to
// change the doc, through the Access Resolver: a doc locked by its owner, in
// the trash, or only shared to read is refused with one answer for "no such
// doc" and "not yours". A suggestion needs only what a comment needs.
//
// RECORDED. Every attempt, allowed or refused, is a row in the hash-chained
// record (docAIRecord.go): ai.doc.agent_edit.allowed or .refused, the agent,
// the asker, the run, the blocks written and replaced, the version kept, and
// a hash of what was written, which is how Revert knows whether anyone has
// edited it since. Never the text. No record, no action: a refusal stands
// whether its row is written or not, and a write that could not be recorded
// is logged loudly (it is already in the doc).
//
// REVERT puts back exactly what was there: the blocks the agent wrote are
// replaced by the blocks they replaced, read from the version kept before the
// write, ids and all; an addition is taken out. When someone has edited what
// the agent wrote since, Revert asks first.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	proposalBusiness "github.com/akashc777/OneCamp/business/Proposal"
	snapshotBusiness "github.com/akashc777/OneCamp/business/Snapshot"
	"github.com/akashc777/OneCamp/helpers"
	agentModel "github.com/akashc777/OneCamp/models/postgres/AIAgent"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

const (
	// docEditMaxContentRunes bounds what one call writes.
	docEditMaxContentRunes = 20000
	// docEditMaxIDs bounds the block ids a row keeps.
	docEditMaxIDs = 200
	// assistantName is how the record names the in-app assistant acting for a
	// person directly, with no agent of its own.
	assistantName = "OneCamp AI"
)

// The ops a row names.
const (
	opAppend         = "append"
	opInsertAfter    = "insert_after"
	opReplaceSection = "replace_section"
	opSuggest        = "suggest"
)

// docEditActor is who a doc edit is from.
type docEditActor struct {
	// RunsAs is the person the tool runs as (an agent's sponsor).
	RunsAs string
	// Asker is the person the edit is for: whoever asked for the run, else
	// RunsAs.
	Asker string
	// Agent is the agent, "" when the assistant acts for the person directly.
	Agent     string
	AgentName string
	RunID     string
}

// agentNameOf is an agent's name: a seam over the store.
var agentNameOf = func(ctx context.Context, agentID string) string {
	id, err := uuid.Parse(agentID)
	if err != nil {
		return ""
	}
	a, err := agentModel.GetAgentByID(ctx, id)
	if err != nil || a == nil {
		return ""
	}
	return strings.TrimSpace(a.Name)
}

// docEditByOf is who a write is credited to: the person the call is for, and
// the agent an agent run or an agent's MCP credential carries, or whose
// proposal the person approved (the write is still the agent's, marked and
// revertable as one).
func docEditByOf(ctx context.Context, userUUID string) collab.By {
	by := collab.By{UserUUID: strings.TrimSpace(userUUID), AgentUUID: strings.TrimSpace(ai.AgentBudgetID(ctx))}
	if by.AgentUUID == "" {
		if id, ok := ai.ApprovedAgentProposal(ctx); ok {
			by.AgentUUID = id
		}
	}
	if requester, _, ok := ai.RunRequester(ctx); ok {
		by.UserUUID = strings.TrimSpace(requester)
	}
	return by
}

// docEditActorOf reads who is acting from the call's context: the agent, its
// name and its run, the person the tool runs as, and the asker.
func docEditActorOf(ctx context.Context, userUUID string) docEditActor {
	by := docEditByOf(ctx, userUUID)
	a := docEditActor{RunsAs: strings.TrimSpace(userUUID), Asker: by.UserUUID, Agent: by.AgentUUID}
	a.RunID = strings.TrimSpace(ai.SandboxScopeFromContext(ctx).RunID)
	switch {
	case a.Agent == "":
		a.AgentName = assistantName
	default:
		// An agent whose name can't be read is still named in the record.
		if a.AgentName = agentNameOf(ctx, a.Agent); a.AgentName == "" {
			a.AgentName = "An agent"
		}
	}
	return a
}

// personByUUID is a person as the record and the Access Resolver need them: a
// seam over the store.
var personByUUID = func(ctx context.Context, userUUID string) (*userModels.UserInfo, error) {
	return getUserInfoForExecutor(ctx, userUUID)
}

// errDocEditUnknownAsker is a run whose asker could not be identified.
var errDocEditUnknownAsker = errors.New("I couldn't tell who asked for this, so I won't change the doc")

// mayChangeDoc decides for every person the edit is for: nil, or why not.
func mayChangeDoc(ctx context.Context, a docEditActor, docUUID string, need accessBusiness.Level) error {
	if a.Asker == "" {
		return errDocEditUnknownAsker
	}
	people := []string{a.Asker}
	if a.RunsAs != "" && !strings.EqualFold(a.RunsAs, a.Asker) {
		people = append(people, a.RunsAs)
	}
	for _, p := range people {
		u, err := personByUUID(ctx, p)
		if err != nil || u == nil {
			return ErrDocAINotFound
		}
		who := accessBusiness.SubjectFor(ctx, u)
		who.AgentUUID = a.Agent
		if _, err := decideDoc(ctx, who, docUUID, need); err != nil {
			return err
		}
	}
	return nil
}

// docEditRefusal is what a tool says when a doc edit is refused.
func docEditRefusal(err error) error {
	switch {
	case errors.Is(err, ErrDocAINotFound):
		return errors.New(helpers.DocNotFound)
	case errors.Is(err, ErrDocAICantEdit):
		return fmt.Errorf("this doc can be read but not edited by the person you're acting for, so I didn't change it")
	}
	return err
}

// recordDocEdit writes an agent edit's row; a refusal's row is best effort.
func recordDocEdit(ctx context.Context, a docEditActor, action, summary string, meta map[string]interface{}) error {
	var actorID *uuid.UUID
	email := ""
	if u, err := personByUUID(ctx, a.Asker); err == nil && u != nil {
		actorID, email = actorOf(u)
	}
	meta["agent_id"] = a.Agent
	meta["agent_name"] = a.AgentName
	meta["asker_uuid"] = a.Asker
	if a.RunID != "" {
		meta["run_id"] = a.RunID
	}
	return recordEntry(context.WithoutCancel(ctx), actorID, email, auditBusiness.ActorAgent, action, summary, meta)
}

func refuseDocEdit(ctx context.Context, a docEditActor, docUUID, op string, why error) error {
	meta := map[string]interface{}{"doc_uuid": docUUID, "op": op, "reason": why.Error()}
	if err := recordDocEdit(ctx, a, ActionDocAgentEditRefused, a.AgentName+" was stopped from changing a doc: "+why.Error(), meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI an agent's refused doc edit on %s could not be recorded: %v", docUUID, err)
	}
	return docEditRefusal(why)
}

// writtenHash is the hash of the blocks with these ids as the doc holds them
// now, in this order; "" when one is missing. Pure.
func writtenHash(blocks []contentcodec.Block, ids []string) string {
	byID := make(map[string]string, len(blocks))
	for _, b := range blocks {
		if b.ID != "" {
			byID[b.ID] = b.HTML
		}
	}
	h := sha256.New()
	for _, id := range ids {
		html, ok := byID[id]
		if !ok {
			return ""
		}
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(html))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func capIDs(ids []string) []string {
	if len(ids) > docEditMaxIDs {
		return ids[:docEditMaxIDs]
	}
	return ids
}

// docEdit is one write an agent tool makes.
type docEdit struct {
	op       string
	docUUID  string
	html     string
	anchorID string   // insert after this top-level block
	replace  []string // replace these top-level blocks
}

// runDocEdit makes the write as a, keeping a version first for an agent, and
// records it.
func runDocEdit(ctx context.Context, a docEditActor, e docEdit) (collab.Result, error) {
	opts := collab.Options{
		By:            collab.By{UserUUID: a.Asker, AgentUUID: a.Agent},
		AgentEdit:     a.Agent != "",
		RunID:         a.RunID,
		SnapshotFirst: a.Agent != "",
	}
	var r collab.Result
	var err error
	switch {
	case len(e.replace) > 0:
		r, err = collab.Live.ReplaceBlocks(ctx, e.docUUID, e.replace, e.html, opts)
	case e.anchorID != "":
		r, err = collab.Live.InsertAfter(ctx, e.docUUID, e.anchorID, e.html, opts)
	default:
		r, err = collab.Live.Append(ctx, e.docUUID, e.html, opts)
	}
	if err != nil {
		return r, liveWriteError(err)
	}
	hash := ""
	if cur, rerr := collab.Read(ctx, e.docUUID); rerr == nil {
		if blocks, berr := contentcodec.Blocks(cur.HTML, contentcodec.BlockOptions{}); berr == nil {
			hash = writtenHash(blocks, r.BlockIDs)
		}
	}
	meta := map[string]interface{}{
		"doc_uuid":     e.docUUID,
		"op":           e.op,
		"edit_id":      r.EditID,
		"block_ids":    capIDs(r.BlockIDs),
		"replaced_ids": capIDs(e.replace),
		"after_id":     e.anchorID,
		"version_id":   r.VersionID,
		"written_hash": hash,
	}
	summary := fmt.Sprintf("%s %s a doc", a.AgentName, docEditVerb(e.op))
	if rerr := recordDocEdit(ctx, a, ActionDocAgentEditAllowed, summary, meta); rerr != nil {
		helpers.LogErrorWithContext(ctx, "business/AI UNRECORDED agent edit: %s wrote into doc %s (edit %s) and the row could not be written: %v", a.AgentName, e.docUUID, r.EditID, rerr)
	}
	return r, nil
}

func docEditVerb(op string) string {
	switch op {
	case opInsertAfter:
		return "added to"
	case opReplaceSection:
		return "replaced a section of"
	case opSuggest:
		return "suggested a change to"
	}
	return "added to the end of"
}

// liveWriteError says a refusal of the Live Doc Writer to the model.
func liveWriteError(err error) error {
	switch {
	case errors.Is(err, collab.ErrDocInTrash):
		return fmt.Errorf("the doc is in the trash, so it takes no changes")
	case errors.Is(err, collab.ErrNoSuchBlock):
		return fmt.Errorf("the doc changed while I was writing and the place I was writing to is gone; read the doc again and retry")
	case errors.Is(err, collab.ErrUnavailable):
		return fmt.Errorf("the document service could not be reached; try again shortly")
	}
	var refused *collab.OpError
	if errors.As(err, &refused) {
		return fmt.Errorf("the document service refused the edit: %s", refused.Msg)
	}
	return err
}

// ---------------------------------------------------------------------------
// Finding where to write
// ---------------------------------------------------------------------------

// normalHeading is a heading as a person names it: no "#" marks, emphasis,
// trailing colon, case or extra spaces. Pure.
func normalHeading(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimLeft(s, "# ")
	s = strings.Trim(s, "*_ ")
	s = strings.TrimRight(s, ":：")
	return strings.ToLower(collapseSpaces(s))
}

// docSection is a heading and the blocks under it, up to the next heading of
// the same or a higher level.
type docSection struct {
	Heading contentcodec.Block
	Blocks  []contentcodec.Block
}

// findSection is the one section whose heading reads as heading. Pure.
func findSection(blocks []contentcodec.Block, heading string) (docSection, error) {
	want := normalHeading(heading)
	if want == "" {
		return docSection{}, fmt.Errorf("name the heading of the section")
	}
	var at []int
	var names []string
	for i, b := range blocks {
		if b.Level == 0 {
			continue
		}
		names = append(names, "“"+b.Text+"”")
		if normalHeading(b.Text) == want {
			at = append(at, i)
		}
	}
	switch len(at) {
	case 0:
		if len(names) == 0 {
			return docSection{}, fmt.Errorf("the doc has no headings, so there is no section called “%s”; add to the end of the doc instead", strings.TrimSpace(heading))
		}
		return docSection{}, fmt.Errorf("there's no heading called “%s” in the doc. Its headings are: %s", strings.TrimSpace(heading), strings.Join(firstN(names, 30), ", "))
	case 1:
	default:
		ids := make([]string, 0, len(at))
		for _, i := range at {
			ids = append(ids, blocks[i].ID)
		}
		return docSection{}, fmt.Errorf("there are %d headings called “%s”; name the one you mean with block_id (their ids, in order: %s)", len(at), strings.TrimSpace(heading), strings.Join(ids, ", "))
	}
	i := at[0]
	s := docSection{Heading: blocks[i]}
	for j := i + 1; j < len(blocks); j++ {
		if blocks[j].Level > 0 && blocks[j].Level <= blocks[i].Level {
			break
		}
		s.Blocks = append(s.Blocks, blocks[j])
	}
	return s, nil
}

func firstN(s []string, n int) []string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// topLevelOf is the top-level block holding the block with id (itself, when it
// is top-level), "" when the doc has none. Pure.
func topLevelOf(blocks []contentcodec.Block, id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return ""
	}
	for _, b := range blocks {
		if b.ID == id {
			return b.ID
		}
	}
	for _, b := range blocks {
		if b.ID != "" && strings.Contains(b.HTML, `data-block-id="`+id+`"`) {
			return b.ID
		}
	}
	return ""
}

func idsOf(blocks []contentcodec.Block) ([]string, bool) {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.ID == "" {
			return nil, false
		}
		out = append(out, b.ID)
	}
	return out, true
}

var errNoBlockIDs = errors.New("this doc's blocks have no ids yet; open it in the app once, then try again")

// docEditPlace reads the live doc's top-level blocks, where an insert or a
// replace goes.
func docEditPlace(ctx context.Context, docUUID string) ([]contentcodec.Block, error) {
	cur, err := collab.Read(ctx, docUUID)
	if err != nil {
		return nil, liveWriteError(err)
	}
	blocks, err := contentcodec.Blocks(cur.HTML, contentcodec.BlockOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w (%v)", errDocUnreadable, err)
	}
	return blocks, nil
}

// ---------------------------------------------------------------------------
// The tools
// ---------------------------------------------------------------------------

func docEditParams(action ai.ProposedAction) (docUUID, content string, err error) {
	docUUID = strings.TrimSpace(action.Params["doc_uuid"])
	if _, perr := uuid.Parse(docUUID); perr != nil {
		return "", "", fmt.Errorf("doc_uuid must be a document's UUID; find it with search_workspace")
	}
	content = strings.TrimSpace(action.Params["content"])
	if content == "" {
		return "", "", fmt.Errorf("content is required")
	}
	if len([]rune(content)) > docEditMaxContentRunes {
		return "", "", fmt.Errorf("content is too long to write at once (%d characters at most); write it in parts", docEditMaxContentRunes)
	}
	return docUUID, content, nil
}

// executeAppendToDoc is append_to_doc: content added at the end of a doc.
func executeAppendToDoc(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	docUUID, content, err := docEditParams(action)
	if err != nil {
		return "", nil, err
	}
	a := docEditActorOf(ctx, userUUID)
	if err := mayChangeDoc(ctx, a, docUUID, accessBusiness.LevelEdit); err != nil {
		return "", nil, refuseDocEdit(ctx, a, docUUID, opAppend, err)
	}
	r, err := runDocEdit(ctx, a, docEdit{op: opAppend, docUUID: docUUID, html: DocHTMLFromMarkdown(content)})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Added %d block(s) to the end of the document. Everything that was already there is unchanged.", r.Blocks),
		map[string]string{"doc_uuid": docUUID, "edit_id": r.EditID}, nil
}

// executeDocInsertAfter is doc_insert_after: content added at the end of a
// heading's section, or right after a block.
func executeDocInsertAfter(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	docUUID, content, err := docEditParams(action)
	if err != nil {
		return "", nil, err
	}
	heading, blockID := strings.TrimSpace(action.Params["heading"]), strings.TrimSpace(action.Params["block_id"])
	if heading == "" && blockID == "" {
		return "", nil, fmt.Errorf("name a heading (its section gets the content at its end) or a block_id")
	}
	a := docEditActorOf(ctx, userUUID)
	if err := mayChangeDoc(ctx, a, docUUID, accessBusiness.LevelEdit); err != nil {
		return "", nil, refuseDocEdit(ctx, a, docUUID, opInsertAfter, err)
	}
	blocks, err := docEditPlace(ctx, docUUID)
	if err != nil {
		return "", nil, err
	}
	anchor, where := "", ""
	if blockID != "" && !isHeadingID(blocks, blockID) {
		if anchor = topLevelOf(blocks, blockID); anchor == "" {
			return "", nil, fmt.Errorf("there's no block %s in the doc; read the doc again", blockID)
		}
		where = "after that block"
	} else {
		// A heading, by its text or its id: the end of its section.
		s, ferr := sectionByHeadingOrID(blocks, heading, blockID)
		if ferr != nil {
			return "", nil, ferr
		}
		last := s.Heading
		if len(s.Blocks) > 0 {
			last = s.Blocks[len(s.Blocks)-1]
		}
		if anchor = last.ID; anchor == "" {
			return "", nil, errNoBlockIDs
		}
		where = "at the end of the “" + s.Heading.Text + "” section"
	}
	r, err := runDocEdit(ctx, a, docEdit{op: opInsertAfter, docUUID: docUUID, html: DocHTMLFromMarkdown(content), anchorID: anchor})
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Added %d block(s) %s. Nothing else in the doc changed.", r.Blocks, where),
		map[string]string{"doc_uuid": docUUID, "edit_id": r.EditID}, nil
}

// executeDocReplaceSection is doc_replace_section: what is under a heading
// replaced, the heading kept.
func executeDocReplaceSection(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	docUUID, content, err := docEditParams(action)
	if err != nil {
		return "", nil, err
	}
	heading, blockID := strings.TrimSpace(action.Params["heading"]), strings.TrimSpace(action.Params["block_id"])
	if heading == "" && blockID == "" {
		return "", nil, fmt.Errorf("name the heading whose section to replace (or its block_id)")
	}
	a := docEditActorOf(ctx, userUUID)
	if err := mayChangeDoc(ctx, a, docUUID, accessBusiness.LevelEdit); err != nil {
		return "", nil, refuseDocEdit(ctx, a, docUUID, opReplaceSection, err)
	}
	blocks, err := docEditPlace(ctx, docUUID)
	if err != nil {
		return "", nil, err
	}
	s, err := sectionByHeadingOrID(blocks, heading, blockID)
	if err != nil {
		return "", nil, err
	}
	e := docEdit{op: opReplaceSection, docUUID: docUUID, html: DocHTMLFromMarkdown(content)}
	if len(s.Blocks) == 0 {
		if e.anchorID = s.Heading.ID; e.anchorID == "" {
			return "", nil, errNoBlockIDs
		}
	} else {
		ids, ok := idsOf(s.Blocks)
		if !ok {
			return "", nil, errNoBlockIDs
		}
		e.replace = ids
	}
	r, err := runDocEdit(ctx, a, e)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("Replaced the “%s” section (%d block(s) out, %d in). The heading and the rest of the doc are unchanged.", s.Heading.Text, len(s.Blocks), r.Blocks),
		map[string]string{"doc_uuid": docUUID, "edit_id": r.EditID}, nil
}

// sectionByHeadingOrID is the section a heading's text or its block id names.
func sectionByHeadingOrID(blocks []contentcodec.Block, heading, blockID string) (docSection, error) {
	if blockID == "" {
		return findSection(blocks, heading)
	}
	for i, b := range blocks {
		if b.ID != blockID {
			continue
		}
		if b.Level == 0 {
			return docSection{}, fmt.Errorf("block %s isn't a heading; name a heading's block_id", blockID)
		}
		s := docSection{Heading: b}
		for j := i + 1; j < len(blocks); j++ {
			if blocks[j].Level > 0 && blocks[j].Level <= b.Level {
				break
			}
			s.Blocks = append(s.Blocks, blocks[j])
		}
		return s, nil
	}
	return docSection{}, fmt.Errorf("there's no block %s in the doc; read the doc again", blockID)
}

// createProposal keeps a suggestion: a seam over A9's service.
var createProposal = func(ctx context.Context, who accessBusiness.Subject, p proposalBusiness.Proposal) (proposalBusiness.Proposal, error) {
	return proposalBusiness.Proposals.Create(ctx, who, p)
}

// executeDocSuggest is doc_suggest: a change proposed for the doc's people to
// review (A9), written nowhere until someone accepts it.
func executeDocSuggest(ctx context.Context, action ai.ProposedAction, userUUID string) (string, map[string]string, error) {
	docUUID, content, err := docEditParams(action)
	if err != nil {
		return "", nil, err
	}
	heading, blockID := strings.TrimSpace(action.Params["heading"]), strings.TrimSpace(action.Params["block_id"])
	mode := strings.ToLower(strings.TrimSpace(action.Params["mode"]))
	if mode == "" {
		mode = "replace"
	}
	if mode != "replace" && mode != "insert_after" {
		return "", nil, fmt.Errorf("mode is replace or insert_after")
	}
	a := docEditActorOf(ctx, userUUID)
	if err := mayChangeDoc(ctx, a, docUUID, accessBusiness.LevelComment); err != nil {
		return "", nil, refuseDocEdit(ctx, a, docUUID, opSuggest, err)
	}
	blocks, err := docEditPlace(ctx, docUUID)
	if err != nil {
		return "", nil, err
	}
	var target proposalBusiness.Target
	var where string
	switch {
	case heading == "" && blockID == "":
		target, where = proposalBusiness.Target{Append: true}, "at the end of the doc"
	case heading != "" || isHeadingID(blocks, blockID):
		s, serr := sectionByHeadingOrID(blocks, heading, blockID)
		if serr != nil {
			return "", nil, serr
		}
		ids, ok := idsOf(s.Blocks)
		switch {
		case !ok:
			return "", nil, errNoBlockIDs
		case mode == "insert_after" && len(ids) > 0:
			target = proposalBusiness.Target{AfterBlockID: ids[len(ids)-1]}
		case len(ids) == 0 || mode == "insert_after":
			target = proposalBusiness.Target{AfterBlockID: s.Heading.ID}
		default:
			target = proposalBusiness.Target{BlockIDs: ids}
		}
		where = "for the “" + s.Heading.Text + "” section"
	default:
		top := topLevelOf(blocks, blockID)
		if top == "" {
			return "", nil, fmt.Errorf("there's no block %s in the doc; read the doc again", blockID)
		}
		if mode == "insert_after" {
			target = proposalBusiness.Target{AfterBlockID: top}
		} else {
			target = proposalBusiness.Target{BlockIDs: []string{top}}
		}
		where = "for that block"
	}
	asker, err := personByUUID(ctx, a.Asker)
	if err != nil || asker == nil {
		return "", nil, refuseDocEdit(ctx, a, docUUID, opSuggest, ErrDocAINotFound)
	}
	who := accessBusiness.SubjectFor(ctx, asker)
	who.AgentUUID = a.Agent
	p := proposalBusiness.Proposal{
		ResourceKind: accessBusiness.KindDoc,
		ResourceUUID: docUUID,
		Note:         helpers.TruncateRunes(strings.TrimSpace(action.Params["note"]), 200),
		RunID:        a.RunID,
		Draft:        &proposalBusiness.Draft{HTML: DocHTMLFromMarkdown(content), Target: target},
	}
	if a.Agent == "" {
		p.AuthorKind = proposalBusiness.AuthorAI
	}
	stored, err := createProposal(ctx, who, p)
	if err != nil {
		switch {
		case errors.Is(err, proposalBusiness.ErrLocked):
			return "", nil, refuseDocEdit(ctx, a, docUUID, opSuggest, errors.New("the doc is locked, so it takes no suggestions"))
		case errors.Is(err, proposalBusiness.ErrForbidden), errors.Is(err, proposalBusiness.ErrNotFound):
			return "", nil, refuseDocEdit(ctx, a, docUUID, opSuggest, ErrDocAINotFound)
		case errors.Is(err, proposalBusiness.ErrTooMany):
			return "", nil, fmt.Errorf("the doc already has too many open suggestions; ask someone to review them first")
		}
		return "", nil, liveWriteError(err)
	}
	meta := map[string]interface{}{"doc_uuid": docUUID, "op": opSuggest, "proposal_id": stored.ID}
	if rerr := recordDocEdit(ctx, a, ActionDocAgentEditAllowed, a.AgentName+" suggested a change to a doc", meta); rerr != nil {
		helpers.LogErrorWithContext(ctx, "business/AI UNRECORDED agent suggestion %s on doc %s: %v", stored.ID, docUUID, rerr)
	}
	return fmt.Sprintf("Suggested a change %s, with %d change(s) for the doc's people to accept or decline. Nothing in the doc changes until someone accepts it.", where, len(stored.Hunks)),
		map[string]string{"doc_uuid": docUUID, "proposal_id": stored.ID}, nil
}

func isHeadingID(blocks []contentcodec.Block, id string) bool {
	if id == "" {
		return false
	}
	for _, b := range blocks {
		if b.ID == id {
			return b.Level > 0
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The record of a doc's agent edits, and Revert
// ---------------------------------------------------------------------------

// AgentEditView is one agent edit on a doc, as the margin chip shows it.
type AgentEditView struct {
	EditID    string    `json:"edit_id"`
	RunID     string    `json:"run_id,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"`
	AgentName string    `json:"agent_name"`
	AskerUUID string    `json:"asker_uuid,omitempty"`
	AskerName string    `json:"asker_name,omitempty"`
	Op        string    `json:"op"`
	BlockIDs  []string  `json:"block_ids"`
	At        time.Time `json:"at"`
	Reverted  bool      `json:"reverted"`
	CanRevert bool      `json:"can_revert"`
}

// agentEditRow is an allowed row's metadata.
type agentEditRow struct {
	DocUUID     string   `json:"doc_uuid"`
	Op          string   `json:"op"`
	EditID      string   `json:"edit_id"`
	RunID       string   `json:"run_id"`
	AgentID     string   `json:"agent_id"`
	AgentName   string   `json:"agent_name"`
	AskerUUID   string   `json:"asker_uuid"`
	BlockIDs    []string `json:"block_ids"`
	ReplacedIDs []string `json:"replaced_ids"`
	AfterID     string   `json:"after_id"`
	VersionID   string   `json:"version_id"`
	Hash        string   `json:"written_hash"`
	EditIDs     []string `json:"edit_ids"`
	at          time.Time
}

// agentEditsOf reads a doc's agent edit rows, newest first, and the edits
// since reverted.
func agentEditsOf(ctx context.Context, docUUID string) ([]agentEditRow, map[string]bool, error) {
	rows, err := listDocAIEntries(ctx, docUUID, "ai.doc.agent_edit.", 200)
	if err != nil {
		return nil, nil, err
	}
	var out []agentEditRow
	reverted := map[string]bool{}
	for _, r := range rows {
		if r == nil || r.Metadata == nil {
			continue
		}
		var m agentEditRow
		if json.Unmarshal([]byte(*r.Metadata), &m) != nil || m.DocUUID != docUUID {
			continue
		}
		switch r.Action {
		case ActionDocAgentEditRevert:
			for _, id := range m.EditIDs {
				reverted[id] = true
			}
		case ActionDocAgentEditAllowed:
			if m.EditID == "" || m.Op == opSuggest {
				continue
			}
			m.at = r.CreatedAt
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.After(out[j].at) })
	return out, reverted, nil
}

// personName is a person's name for the margin chip: a seam.
var personName = func(ctx context.Context, userUUID string) string {
	u, err := personByUUID(ctx, userUUID)
	if err != nil || u == nil {
		return ""
	}
	if n := strings.TrimSpace(u.UserDgraphInfo.UserFullName); n != "" {
		return n
	}
	return strings.TrimSpace(u.UserDgraphInfo.UserName)
}

// ListAgentEdits is a doc's agent edits, newest first, for someone who can
// read it; CanRevert says whether they may revert each one.
func ListAgentEdits(ctx context.Context, u *userModels.UserInfo, docUUID string) ([]AgentEditView, error) {
	docUUID = strings.TrimSpace(docUUID)
	d, err := checkDocQuietly(ctx, u, docUUID, accessBusiness.LevelView)
	if err != nil {
		return nil, err
	}
	rows, reverted, err := agentEditsOf(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	canEdit := canEditDecision(d)
	names := map[string]string{}
	out := make([]AgentEditView, 0, len(rows))
	for _, r := range rows {
		if _, ok := names[r.AskerUUID]; !ok && r.AskerUUID != "" {
			names[r.AskerUUID] = personName(ctx, r.AskerUUID)
		}
		agent := r.AgentName
		if agent == "" {
			agent = assistantName
		}
		out = append(out, AgentEditView{
			EditID: r.EditID, RunID: r.RunID, AgentID: r.AgentID, AgentName: agent,
			AskerUUID: r.AskerUUID, AskerName: names[r.AskerUUID], Op: r.Op, BlockIDs: r.BlockIDs,
			At: r.at, Reverted: reverted[r.EditID], CanRevert: canEdit && !reverted[r.EditID],
		})
	}
	return out, nil
}

// EditedSinceError is a revert of something people have edited since the
// agent wrote it.
type EditedSinceError struct{ Unknown bool }

func (e *EditedSinceError) Error() string {
	if e.Unknown {
		return "I can't tell whether anyone has edited what the agent wrote since. Revert anyway, to put back what was there before?"
	}
	return "Someone has edited what the agent wrote since. Revert anyway, to put back what was there before?"
}

// versionHTML is the doc as a version keeps it: a seam over the Snapshot
// Service, asked as who.
var versionHTML = func(ctx context.Context, who accessBusiness.Subject, docUUID, versionID string) (string, error) {
	return snapshotBusiness.Snapshots.Preview(ctx, who, accessBusiness.KindDoc, docUUID, versionID)
}

// RevertError is a revert that can't be made, said to the person.
type RevertError struct{ Msg string }

func (e *RevertError) Error() string { return e.Msg }

// errVersionGone is a revert whose version kept before the write is gone.
var errVersionGone = &RevertError{"The version from before this edit is no longer kept, so it can't be put back exactly. Restore an earlier version from the doc's history instead."}

// RevertAgentEdits reverts a run's edits on a doc (runID), or one edit
// (editID), newest first, as u, who must be able to edit the doc. force goes
// ahead when people have edited the agent's text since. It answers how many
// edits were reverted.
func RevertAgentEdits(ctx context.Context, u *userModels.UserInfo, docUUID, runID, editID string, force bool) (int, error) {
	docUUID, runID, editID = strings.TrimSpace(docUUID), strings.TrimSpace(runID), strings.TrimSpace(editID)
	if runID == "" && editID == "" {
		return 0, &DocAIInvalidError{"Name the edit or the run to revert."}
	}
	if _, err := CheckDocForAI(ctx, u, docUUID, accessBusiness.LevelEdit); err != nil {
		return 0, err
	}
	rows, reverted, err := agentEditsOf(ctx, docUUID)
	if err != nil {
		return 0, err
	}
	var todo []agentEditRow
	for _, r := range rows {
		if reverted[r.EditID] || (editID != "" && r.EditID != editID) || (editID == "" && r.RunID != runID) {
			continue
		}
		todo = append(todo, r)
	}
	if len(todo) == 0 {
		return 0, &DocAIInvalidError{"There's nothing of that edit left to revert."}
	}
	who := accessBusiness.SubjectFor(ctx, u)
	by := collab.By{UserUUID: u.UserDgraphInfo.Uuid}
	done := make([]string, 0, len(todo))
	for _, r := range todo {
		if err := revertOne(ctx, who, by, docUUID, r, force); err != nil {
			if len(done) > 0 {
				recordRevert(ctx, u, docUUID, runID, done)
			}
			return len(done), err
		}
		done = append(done, r.EditID)
	}
	recordRevert(ctx, u, docUUID, runID, done)
	return len(done), nil
}

func revertOne(ctx context.Context, who accessBusiness.Subject, by collab.By, docUUID string, r agentEditRow, force bool) error {
	cur, err := collab.Read(ctx, docUUID)
	if err != nil {
		return &RevertError{revertSentence(liveWriteError(err))}
	}
	blocks, err := contentcodec.Blocks(cur.HTML, contentcodec.BlockOptions{})
	if err != nil {
		return &RevertError{revertSentence(errDocUnreadable)}
	}
	present := map[string]bool{}
	for _, b := range blocks {
		if b.ID != "" {
			present[b.ID] = true
		}
	}
	var still []string
	for _, id := range r.BlockIDs {
		if present[id] {
			still = append(still, id)
		}
	}
	if !force {
		switch {
		case r.Hash == "":
			return &EditedSinceError{Unknown: true}
		case len(still) != len(r.BlockIDs) || writtenHash(blocks, r.BlockIDs) != r.Hash:
			return &EditedSinceError{}
		}
	}
	prior := ""
	if len(r.ReplacedIDs) > 0 {
		if r.VersionID == "" {
			return errVersionGone
		}
		before, verr := versionHTML(ctx, who, docUUID, r.VersionID)
		if verr != nil {
			return errVersionGone
		}
		kept, berr := contentcodec.Blocks(before, contentcodec.BlockOptions{})
		if berr != nil {
			return errVersionGone
		}
		prior, err = blocksHTML(kept, r.ReplacedIDs)
		if err != nil {
			return err
		}
	}
	opts := collab.Options{By: by, SnapshotFirst: true, SnapshotReason: "before_write"}
	switch {
	case len(still) > 0:
		_, err = collab.Live.ReplaceBlocks(ctx, docUUID, still, prior, opts)
	case prior != "" && present[r.AfterID]:
		_, err = collab.Live.InsertAfter(ctx, docUUID, r.AfterID, prior, opts)
	case prior != "":
		return &RevertError{"What the agent wrote is gone, and so is the place it was. Restore an earlier version from the doc's history instead."}
	}
	if err != nil {
		return &RevertError{revertSentence(liveWriteError(err))}
	}
	return nil
}

// revertSentence is a live write's refusal as a sentence for the person.
func revertSentence(err error) string {
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "The revert couldn't be made just now. Try again in a moment."
	}
	r := []rune(msg)
	return strings.ToUpper(string(r[0])) + string(r[1:]) + "."
}

// blocksHTML is the blocks with these ids, in this order, as one HTML. Pure.
func blocksHTML(blocks []contentcodec.Block, ids []string) (string, error) {
	byID := make(map[string]string, len(blocks))
	for _, b := range blocks {
		if b.ID != "" {
			byID[b.ID] = b.HTML
		}
	}
	var sb strings.Builder
	for _, id := range ids {
		html, ok := byID[id]
		if !ok {
			return "", errVersionGone
		}
		sb.WriteString(html)
	}
	return sb.String(), nil
}

func recordRevert(ctx context.Context, u *userModels.UserInfo, docUUID, runID string, editIDs []string) {
	id, email := actorOf(u)
	meta := map[string]interface{}{"doc_uuid": docUUID, "edit_ids": editIDs}
	if runID != "" {
		meta["run_id"] = runID
	}
	summary := fmt.Sprintf("Reverted %d agent %s in a doc", len(editIDs), plural2(len(editIDs), "edit", "edits"))
	if err := recordEntry(context.WithoutCancel(ctx), id, email, auditBusiness.ActorHuman, ActionDocAgentEditRevert, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI a revert on doc %s could not be recorded: %v", docUUID, err)
	}
}
