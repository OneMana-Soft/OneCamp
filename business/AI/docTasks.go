package business

// Doc → tasks (docs programme B9, ai-search-io P0-6): "Find action items" reads
// a doc and proposes real tasks, each with an owner, a due date and the line it
// came from; on Accept they are made in a project the person may add tasks to,
// each links back to its line, and the doc gets a task chip there.
//
// Notion turns action items into rows only when a meeting-notes database is set
// up. Here they are tasks in the projects people already work in.
//
// THE LINE A TASK CAME FROM. The doc is given to the model one line per text
// block, each with its number ("[3] Priya to send the deck by Friday"), and the
// model names the number with each task. A small model gets that wrong now and
// then, so a number that doesn't fit is replaced by the line whose words match
// the task best, and a task that matches no line well has none: its chip is put
// nowhere rather than in the wrong place.
//
// ASKER-BOUND. Everything is done AS the person: the doc must be one they can
// read, the project one they are an admin of (the task path's own rule), and a
// request for any other project is refused and recorded, whoever forged it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	entityLinkBusiness "github.com/akashc777/OneCamp/business/EntityLink"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// DocProposedTask is a proposed task with the line it came from.
type DocProposedTask struct {
	ProposedTask
	// BlockID is the block of the line it came from ("" when no line matched).
	BlockID string `json:"block_id,omitempty"`
	// Quote is that line, shortened, so the review can show where it is.
	Quote string `json:"quote,omitempty"`
}

// DocTasksResult is what "Find action items" proposes.
type DocTasksResult struct {
	Enabled bool              `json:"enabled"`
	Tasks   []DocProposedTask `json:"tasks"`
	Note    string            `json:"note,omitempty"`
	// Lines is how many lines of the doc were read.
	Lines int `json:"lines,omitempty"`
	// Project is the doc's own project when the person is in it: the one the
	// review picks first.
	Project *DocProject `json:"project,omitempty"`
}

// docBlocksForTasks reads a doc's lines for someone who can read it: its
// blocks of text wherever they are (paragraphs, headings, list items' lines,
// code), each with its block id, as the content codec reads them. A ticked
// to-do is left out: it is not an action item any more.
func docBlocksForTasks(ctx context.Context, u *userModels.UserInfo, docUUID string) ([]contentcodec.Block, error) {
	if _, err := CheckDocForAI(ctx, u, docUUID, accessBusiness.LevelView); err != nil {
		return nil, err
	}
	src, err := liveDocHTML(ctx, strings.TrimSpace(docUUID))
	if err != nil {
		return nil, err
	}
	return contentcodec.Blocks(src, contentcodec.BlockOptions{Text: true, SkipDone: true})
}

// numberedBlocks is the doc as the model reads it, a numbered line a block,
// whole lines up to extractMaxSourceChars characters. Pure.
func numberedBlocks(lines []contentcodec.Block) string {
	var b strings.Builder
	n := 0
	for i, l := range lines {
		line := fmt.Sprintf("[%d] %s\n", i+1, l.Text)
		if n += utf8.RuneCountInString(line); n > extractMaxSourceChars {
			break
		}
		b.WriteString(line)
	}
	return strings.TrimSpace(b.String())
}

// docTaskSource is a doc the person can read, one numbered line a block of
// text, so the model can say which line each action item came from. lines,
// when given, keeps the lines read.
func docTaskSource(u *userModels.UserInfo, docUUID string, lines *[]contentcodec.Block) taskSource {
	return taskSource{
		read: func(ctx context.Context) (string, int, error) {
			blocks, err := docBlocksForTasks(ctx, u, docUUID)
			if err != nil {
				return "", 0, err
			}
			if lines != nil {
				*lines = blocks
			}
			return numberedBlocks(blocks), len(blocks), nil
		},
		prompt:    docTaskExtractionPrompt,
		noun:      "Document",
		maxTokens: 1400,
		tooShort:  "There isn't enough in this doc to find action items in yet.",
		noneFound: "No clear action items in this doc.",
	}
}

const docTaskExtractionPrompt = `You extract ACTION ITEMS from a document and turn them into tasks.
The document is given one block per line. Each line starts with its number in brackets, like [3].
Return ONLY a JSON object (no prose, no markdown fences) of this shape:
{"tasks": [
  {
    "title": "<short imperative task title>",
    "assignee": "<person name if a specific owner is stated, else empty>",
    "due": "<YYYY-MM-DD if a deadline is stated, else empty>",
    "priority": "urgent" | "high" | "medium" | "low",
    "details": "<one short line of context, optional>",
    "block": <the number of the line the action item comes from>
  }
]}

Rules:
- Extract only CONCRETE things someone should DO: commitments, follow-ups, requests and open to-dos. Ignore decisions, background, finished work and questions that only ask for information.
- "Priya to send the deck by Friday" is an action item: title "Send the deck", assignee "Priya", due that Friday.
- Use ONLY what the document says. Never invent owners, dates or work.
- Title is a short imperative ("Draft the pricing doc"), not the line copied word for word.
- Resolve relative dates with the calendar given; otherwise leave "due" empty.
- Return at most 20 items. If there are no clear action items, return exactly: {"tasks": []}
- Output MUST be valid JSON. No trailing commas, no comments.`

var firstNumber = regexp.MustCompile(`\d+`)

// blockNumber reads the line number a model gave, written as 3, "3" or "[3]";
// 0 when there is none. Pure.
func blockNumber(raw json.RawMessage) int {
	m := firstNumber.FindString(string(raw))
	if m == "" {
		return 0
	}
	n, err := strconv.Atoi(m)
	if err != nil {
		return 0
	}
	return n
}

// ExtractDocActionItems proposes the action items in a doc the person can
// read, each on the line it came from: the one extraction flow
// (extractTasks) over the doc's numbered lines. Read-only: nothing is made
// until CreateDocTasks.
func ExtractDocActionItems(ctx context.Context, u *userModels.UserInfo, docUUID string) (*DocTasksResult, error) {
	var lines []contentcodec.Block
	x, err := extractTasks(ctx, u, docTaskSource(u, docUUID, &lines))
	if err != nil {
		return nil, err
	}
	resp := &DocTasksResult{Enabled: x.enabled, Note: x.note, Lines: x.scanned, Tasks: make([]DocProposedTask, 0, len(x.tasks))}
	if x.enabled {
		resp.Project = docProjectFor(ctx, u, docUUID)
	}
	for _, t := range x.tasks {
		pt := DocProposedTask{ProposedTask: t.ProposedTask}
		if line, ok := lineFor(lines, blockNumber(t.raw.Block), t.raw.Title+" "+t.raw.Details); ok {
			pt.BlockID, pt.Quote = line.ID, helpers.TruncateRunes(line.Text, 160)
		}
		resp.Tasks = append(resp.Tasks, pt)
	}
	return resp, nil
}

// lineFor is the line a task came from: the line the model numbered when its
// words fit the task as well as any line's do, else the line that fits best,
// else none. Pure.
func lineFor(lines []contentcodec.Block, n int, task string) (contentcodec.Block, bool) {
	want := wordSet(task)
	if len(want) == 0 {
		return contentcodec.Block{}, false
	}
	best, bestScore := -1, 0.0
	for i, l := range lines {
		if s := overlap(want, wordSet(l.Text)); s > bestScore {
			best, bestScore = i, s
		}
	}
	if n >= 1 && n <= len(lines) {
		if s := overlap(want, wordSet(lines[n-1].Text)); s > 0 && s >= bestScore {
			return lines[n-1], true
		}
	}
	if best < 0 || bestScore < 0.34 {
		return contentcodec.Block{}, false
	}
	return lines[best], true
}

// stopWords say nothing about which line a task came from.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true, "this": true,
	"will": true, "are": true, "was": true, "into": true, "our": true, "you": true, "your": true,
	"its": true, "has": true, "have": true, "not": true, "but": true, "all": true, "any": true,
	"can": true, "out": true, "who": true, "get": true, "set": true, "off": true, "per": true,
}

// wordSet is the words of three letters or more in s that say something,
// lower-cased.
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }) {
		if len([]rune(w)) >= 3 && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// overlap is the share of want's words that are in have.
func overlap(want, have map[string]bool) float64 {
	if len(want) == 0 {
		return 0
	}
	n := 0
	for w := range want {
		if have[w] {
			n++
		}
	}
	return float64(n) / float64(len(want))
}

// DocTasksCreated is what CreateDocTasks made.
type DocTasksCreated struct {
	Created int              `json:"created"`
	Failed  int              `json:"failed"`
	Tasks   []DocCreatedTask `json:"tasks"`
	Project DocProject       `json:"project"`
}

// DocCreatedTask is one task made from a doc, and the line its chip goes on.
type DocCreatedTask struct {
	TaskUUID string `json:"task_uuid"`
	Title    string `json:"title"`
	BlockID  string `json:"block_id,omitempty"`
}

// CantAddTasksError is a project the person may not add tasks to. Name is the
// project's name when they can see it, else "".
type CantAddTasksError struct{ Name string }

func (e *CantAddTasksError) Error() string {
	if e.Name != "" {
		return "You can't add tasks to " + e.Name + ", so none were made."
	}
	return "You can't add tasks to that project, so none were made."
}

// projectForTasks is the project as the person sees it: a seam for tests.
var projectForTasks = func(ctx context.Context, projectUUID, userUID string) (*dgraphStruct.DgraphProject, error) {
	return projectDomain.GetBasicDgraphProjectInfo(ctx, projectUUID, userUID)
}

// makeDocTask makes one task: a seam for tests (createProposedTask).
var makeDocTask = createProposedTask

// standingIn is how the person stands to the project (Work Access): a seam
// for tests.
var standingIn = workAccess.Of

// linkTaskToDoc lists the doc on the task: a seam for tests.
var linkTaskToDoc = func(ctx context.Context, taskUUID, docUUID string) error {
	return entityLinkBusiness.AddLink(ctx, entityLinkBusiness.SourceTask, taskUUID, entityLinkBusiness.RefDoc, docUUID)
}

// CreateDocTasks makes the chosen tasks in a project the person may add tasks
// to (Work Access), AS them, each linking back to the doc and its line. A
// project they may not add tasks to is refused and recorded; one they aren't
// in is refused without its name.
func CreateDocTasks(ctx context.Context, u *userModels.UserInfo, docUUID, projectUUID string, tasks []DocProposedTask) (*DocTasksCreated, error) {
	docUUID = strings.TrimSpace(docUUID)
	if len(tasks) == 0 {
		return nil, &DocAIInvalidError{"Pick at least one task to make."}
	}
	if len(tasks) > extractMaxTasks {
		tasks = tasks[:extractMaxTasks]
	}
	if _, err := CheckDocForAI(ctx, u, docUUID, accessBusiness.LevelView); err != nil {
		return nil, err
	}
	projectParsed, perr := uuid.Parse(strings.TrimSpace(projectUUID))
	refuse := func(name, reason string) error {
		id, email := actorOf(u)
		meta := map[string]interface{}{"doc_uuid": docUUID, "project_uuid": strings.TrimSpace(projectUUID), "reason": reason, "tasks": len(tasks)}
		if err := recordEntry(context.WithoutCancel(ctx), id, email, auditBusiness.ActorHuman, ActionDocAITasksRefused,
			"Doc AI refused to make tasks: "+reason, meta); err != nil {
			helpers.LogErrorWithContext(ctx, "business/AI tasks refusal on %s could not be recorded: %v", docUUID, err)
		}
		return &CantAddTasksError{Name: name}
	}
	if perr != nil {
		return nil, refuse("", "not a project")
	}
	project, err := projectForTasks(ctx, projectParsed.String(), u.UserDgraphInfo.Uid)
	if err != nil || project == nil || project.Uuid == "" {
		return nil, refuse("", "no such project, or the person can't see it")
	}
	if !project.ReaderIsIn() {
		return nil, refuse("", "no such project, or the person can't see it")
	}
	standing, err := standingIn(ctx, u.UserDgraphInfo.Uuid, project)
	if err != nil {
		return nil, err
	}
	if !standing.Can(workAccess.NeedCreateTask) {
		return nil, refuse(strings.TrimSpace(project.Name), "the person can't add tasks to this project")
	}

	title := strings.TrimSpace(DocTitleFor(ctx, docUUID, u.UserDgraphInfo.Uid))
	out := &DocTasksCreated{Tasks: []DocCreatedTask{}, Project: DocProject{UUID: project.Uuid, Name: strings.TrimSpace(project.Name)}}
	for _, t := range tasks {
		taskUUID, cerr := makeDocTask(ctx, u, projectParsed, project, t.ProposedTask, docTaskDescription(docUUID, t.BlockID, title, t.Description))
		if cerr != nil {
			out.Failed++
			continue
		}
		out.Created++
		if lerr := linkTaskToDoc(ctx, taskUUID.String(), docUUID); lerr != nil {
			helpers.LogErrorWithContext(ctx, "business/AI could not list doc %s on task %s: %v", docUUID, taskUUID, lerr)
		}
		out.Tasks = append(out.Tasks, DocCreatedTask{TaskUUID: taskUUID.String(), Title: strings.TrimSpace(t.Title), BlockID: strings.TrimSpace(t.BlockID)})
	}
	if out.Created == 0 {
		return out, errors.New("none of the tasks could be made; try again in a moment")
	}
	ids := make([]string, 0, len(out.Tasks))
	for _, t := range out.Tasks {
		ids = append(ids, t.TaskUUID)
	}
	id, email := actorOf(u)
	summary := fmt.Sprintf("Made %d %s in %s from a doc's action items", out.Created, plural2(out.Created, "task", "tasks"), out.Project.Name)
	meta := map[string]interface{}{"doc_uuid": docUUID, "project_uuid": out.Project.UUID, "tasks": ids, "created": out.Created, "failed": out.Failed}
	if err := recordEntry(ctx, id, email, auditBusiness.ActorHuman, ActionDocAITasksCreated, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/AI tasks made from doc %s could not be recorded: %v", docUUID, err)
	}
	return out, nil
}

func plural2(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// docTaskDescription is a task's description: where it came from, as a link
// to the doc and its line, then the line's context. Pure.
func docTaskDescription(docUUID, blockID, docTitle, details string) string {
	name := strings.TrimSpace(docTitle)
	if name == "" {
		name = "a doc"
	}
	href := "/app/doc/" + docUUID
	if blockID = strings.TrimSpace(blockID); blockID != "" {
		href += "#block-" + blockID
	}
	var b strings.Builder
	b.WriteString(`<p>From <a href="` + html.EscapeString(href) + `">` + html.EscapeString(name) + `</a></p>`)
	if d := strings.TrimSpace(details); d != "" {
		b.WriteString("<p>" + html.EscapeString(d) + "</p>")
	}
	return b.String()
}
