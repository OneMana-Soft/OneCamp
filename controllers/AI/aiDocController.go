package controllers

// Doc AI (docs programme B9): the doc's AI bar runs its actions here.
//
// POST /ai/doc/complete/stream streams the answer as Server-Sent Events, one
// JSON object a frame, in this order:
//
//	{"model": {...}}     which model writes, before anything else: the bar
//	                     says "Runs on your server · qwen3 4B", or names the
//	                     provider a remote model is at, before the first word
//	{"progress": {...}}  a long doc's summary reading it part by part
//	{"content": "..."}   the answer as it is written
//	{"replace": "..."}   the whole answer, cleaned, when the cleaning at the end
//	                     changed what was streamed
//	{"notice": "..."}    the input was shortened to fit the model
//	{"error": "..."} | {"done": true}
//
// and a ": ping" comment every 10 seconds, so a proxy never closes a stream
// while a local model is still working on its first word.
//
// POST /ai/doc/complete answers the same request in one piece, and GET
// /doc/ai/model names the model before anything is asked.
//
// The actions, what each reads and the prompts are business/AI docAI.go. A doc
// named by doc_id must be one the person can read; a refusal is recorded.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	business "github.com/akashc777/OneCamp/business/AI"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// DocAIRequest is one doc AI action.
type DocAIRequest struct {
	// Action is improve | fix | shorter | longer | tone | translate | simplify |
	// explain | summarize | continue | write | draft | draft_from_tasks | ask
	// (the old panel's rewrite, fix_grammar, shorten and expand still work).
	Action string `json:"action"`
	// Text is the selection, as Markdown.
	Text string `json:"text"`
	// Prompt is the person's instruction, refinement or question.
	Prompt string `json:"prompt"`
	// DocID is the doc the action is in.
	DocID string `json:"doc_id"`
	// Context is the text around the selection, or before the caret.
	Context string `json:"context"`
	// Tone is the tone for "tone": professional | casual | direct | confident | friendly.
	Tone string `json:"tone"`
	// Language is the language for "translate", as a person names it.
	Language string `json:"language"`
	// Scope "doc" runs the action on the whole doc (summarise it, translate it).
	Scope string `json:"scope"`
	// DocText is the doc as Markdown from the live editor, typing included;
	// without it the collaboration service's live copy is read.
	DocText string `json:"doc_text"`
	// Previous is the answer being refined.
	Previous string `json:"previous"`
	// ProjectUUID is the project for draft_from_tasks (the doc's own when empty).
	ProjectUUID string `json:"project_uuid"`
}

// DocAIResponse is the answer of POST /ai/doc/complete.
type DocAIResponse struct {
	Result   string      `json:"result"`
	Action   string      `json:"action"`
	Provider string      `json:"provider"`
	Model    ai.DocModel `json:"model"`
	// Notice is set when the input was shortened to fit the model's window.
	Notice string `json:"notice,omitempty"`
}

// docAIRuntime is what doc AI asks of the AI service: a seam, so the stream
// is tested with a fake model.
type docAIRuntime interface {
	Enabled() bool
	CheckRateLimit(ctx context.Context, userUUID string) error
	// Resolve is the member's model, its breaker and limits, and its name.
	Resolve(ctx context.Context, userUUID string) (ai.LLMProvider, *ai.CircuitBreaker, ai.ModelLimits, ai.DocModel)
}

type serviceRuntime struct{}

func (serviceRuntime) Enabled() bool { return ai.GetService().IsEnabled() }

func (serviceRuntime) CheckRateLimit(ctx context.Context, userUUID string) error {
	svc := ai.GetService()
	if svc == nil || svc.Resiliency == nil {
		return nil
	}
	return svc.Resiliency.CheckRateLimit(ctx, userUUID)
}

func (serviceRuntime) Resolve(ctx context.Context, userUUID string) (ai.LLMProvider, *ai.CircuitBreaker, ai.ModelLimits, ai.DocModel) {
	svc := ai.GetService()
	llm, cb, limits := svc.ResolveUserModelWithLimits(ctx, userUUID)
	return llm, cb, limits, svc.DescribeModel(ctx, userUUID, llm)
}

var docAIRun docAIRuntime = serviceRuntime{}

// docAIPingEvery keeps a slow stream open through proxies.
var docAIPingEvery = 10 * time.Second

// maxDocAIInputRunes bounds what the client may send as text, context or doc.
const maxDocAIInputRunes = 200_000

// preparedDocAI is a validated action, ready to run.
type preparedDocAI struct {
	action business.DocAction
	in     business.DocAIInput
	llm    ai.LLMProvider
	cb     *ai.CircuitBreaker
	limits ai.ModelLimits
	model  ai.DocModel
	// parts are a long doc's parts for the map step of its summary.
	parts  []string
	cut    bool
	budget int
	opts   ai.ChatOptions
	// source is the text the answer is cleaned against (CleanDocAnswer).
	source string
	// trimmed: the selection or the doc was shortened to fit the model.
	trimmed bool
}

// docAIError is a refusal with its status, said to the person.
type docAIError struct {
	status int
	msg    string
}

func (e *docAIError) Error() string { return e.msg }

func badDocAI(msg string) error { return &docAIError{status: http.StatusBadRequest, msg: msg} }

func clipRunes(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > maxDocAIInputRunes {
		r = r[:maxDocAIInputRunes]
	}
	return string(r)
}

// prepareDocAI validates a request and gathers what the action reads.
func prepareDocAI(ctx context.Context, userInfo *userModels.UserInfo, req DocAIRequest) (context.Context, *preparedDocAI, error) {
	action, ok := business.LookupDocAction(req.Action)
	if !ok {
		return ctx, nil, badDocAI("That isn't an action the AI bar knows.")
	}
	in := business.DocAIInput{
		Action: action, Text: clipRunes(req.Text), Prompt: clipRunes(req.Prompt), Context: clipRunes(req.Context),
		Tone: strings.ToLower(strings.TrimSpace(req.Tone)), Language: strings.TrimSpace(req.Language),
		Previous: clipRunes(req.Previous),
	}
	wholeDoc := strings.EqualFold(strings.TrimSpace(req.Scope), "doc")
	switch {
	case action.Name == "tone" && business.DocTones[in.Tone] == "":
		return ctx, nil, badDocAI("Pick a tone: professional, casual, direct, confident or friendly.")
	case action.Name == "translate" && !business.ValidDocLanguage(in.Language):
		return ctx, nil, badDocAI("Name the language to translate into, like French or Japanese.")
	case action.NeedsText && in.Text == "" && !wholeDoc:
		return ctx, nil, badDocAI("Select some text first, then ask again.")
	case action.NeedsPrompt && in.Prompt == "":
		switch action.Name {
		case "ask":
			return ctx, nil, badDocAI("Type a question about the doc.")
		case "edit":
			return ctx, nil, badDocAI("Say what to change.")
		}
		return ctx, nil, badDocAI("Say what to write.")
	}

	docID := strings.TrimSpace(req.DocID)
	readsDoc := action.ReadsDoc || (wholeDoc && in.Text == "") ||
		(action.Name == "summarize" && in.Text == "") ||
		(action.Name == "continue" && in.Text == "" && in.Context == "") ||
		(action.Name == "translate" && in.Text == "")
	if docID != "" {
		// What the action needs on the doc (business/AI DocActionNeeds, the
		// list /ai/status gives the web): reading needs view, changing edit
		// (docs incident, 14 Oct 2026, D1).
		d, err := business.CheckDocForAI(ctx, userInfo, docID, business.DocActionNeeds(action.Name))
		if err != nil {
			if errors.Is(err, business.ErrDocAINotFound) {
				return ctx, nil, &docAIError{status: http.StatusNotFound, msg: helpers.DocNotFound}
			}
			if errors.Is(err, business.ErrDocAICantEdit) {
				verb := "view"
				if d.Level == accessBusiness.LevelComment {
					verb = "comment on"
				}
				return ctx, nil, &docAIError{status: http.StatusForbidden, msg: "You can " + verb + " this doc, so Ask AI can read it with you but not change it."}
			}
			return ctx, nil, err
		}
	} else if readsDoc && strings.TrimSpace(req.DocText) == "" {
		return ctx, nil, badDocAI("Open a doc first: this action reads the doc.")
	}

	userUUID := userInfo.UserDgraphInfo.Uuid
	llm, cb, limits, model := docAIRun.Resolve(ctx, userUUID)
	if llm == nil || cb == nil {
		return ctx, nil, &docAIError{status: http.StatusServiceUnavailable, msg: "AI is switched off in this workspace."}
	}
	ctx = ai.WithModelLimits(ctx, limits)
	ctx = ai.WithContextNoticeSink(ctx)
	ai.NoteQuestion(ctx, strings.TrimSpace(in.Prompt+" "+in.Text))

	p := &preparedDocAI{action: action, llm: llm, cb: cb, limits: limits, model: model, opts: docAIOptions(action)}
	p.budget = limits.DocInputBudget(p.opts.MaxTokens)

	if docID != "" && (readsDoc || action.Name == "write" || action.Name == "draft" || action.Name == "summarize") {
		in.DocTitle = business.DocTitleFor(ctx, docID, userInfo.UserDgraphInfo.Uid)
	}
	if readsDoc {
		md := clipRunes(req.DocText)
		if md == "" {
			html, err := business.ReadLiveDocHTML(ctx, docID)
			if err != nil {
				return ctx, nil, &docAIError{status: http.StatusServiceUnavailable, msg: "The doc's text couldn't be read just now. Try again in a moment."}
			}
			md = business.DocMarkdown(html)
		}
		if strings.TrimSpace(md) == "" && action.Name != "continue" {
			return ctx, nil, badDocAI("This doc is empty, so there's nothing to read yet.")
		}
		if action.Name == "summarize" {
			if parts, mapped, cut := business.PlanDocSummary(md, limits, p.budget); mapped {
				p.parts, p.cut = parts, cut
				md = ""
			}
		}
		if md != "" {
			whole := md
			md = limits.TruncateForPrompt(ctx, md, p.budget)
			p.trimmed = p.trimmed || md != whole
		}
		in.DocMarkdown = md
	}
	if action.Name == "draft_from_tasks" {
		project := strings.TrimSpace(req.ProjectUUID)
		if project == "" && docID != "" {
			if s, err := business.DocAIStarters(ctx, userInfo, docID); err == nil && s.Project != nil {
				project = s.Project.UUID
			}
		}
		if project == "" {
			return ctx, nil, badDocAI("Pick a project to draft from.")
		}
		name, tasks, n, err := business.ProjectOpenTasksText(ctx, userInfo, project)
		if errors.Is(err, business.ErrNotInProject) {
			business.RecordDocAIRefusal(ctx, userInfo, docID, business.ActionDocAIReadRefused, err.Error())
			return ctx, nil, &docAIError{status: http.StatusForbidden, msg: "You aren't in that project, so its tasks can't be read."}
		}
		if err != nil {
			return ctx, nil, err
		}
		if n == 0 {
			return ctx, nil, badDocAI("That project has no open tasks to draft from.")
		}
		in.Tasks = limits.TruncateForPrompt(ctx, "Project: "+name+"\n"+tasks, p.budget)
	}
	// What the person sent is bounded too, after the doc: the selection
	// visibly (a notice says so), and the text around it by keeping what is
	// nearest the caret.
	before := in.Text
	in.Text = limits.TruncateForPrompt(ctx, in.Text, p.budget)
	p.trimmed = p.trimmed || in.Text != before
	in.Context = keepTail(limits, in.Context, p.budget/4)
	p.source = helpers.FirstNonBlank(in.Text, in.Previous, in.Context, in.DocMarkdown)
	p.in = in
	return ctx, p, nil
}

// docAIOptions is how a model is asked for an action's answer. Thinking stays
// at OneCamp's default (off): qwen3:4b-instruct refuses a request that asks for
// it.
func docAIOptions(a business.DocAction) ai.ChatOptions {
	switch a.Name {
	case "fix":
		return ai.ChatOptions{Temperature: 0.1, MaxTokens: 2048}
	case "continue":
		return ai.ChatOptions{Temperature: 0.7, MaxTokens: 700}
	case "write", "draft", "draft_from_tasks":
		return ai.ChatOptions{Temperature: 0.6, MaxTokens: 2048}
	case "explain", "ask":
		return ai.ChatOptions{Temperature: 0.3, MaxTokens: 1024}
	}
	return ai.ChatOptions{Temperature: 0.4, MaxTokens: 2048}
}

// gateDocAI decodes a request and checks the service and the rate limit,
// answering a refusal itself. ok is false when it answered.
func gateDocAI(w http.ResponseWriter, r *http.Request, ctx context.Context) (DocAIRequest, *userModels.UserInfo, bool) {
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !docAIRun.Enabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "AI is switched off in this workspace."})
		return DocAIRequest{}, nil, false
	}
	var req DocAIRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		helpers.WriteJSON(w, http.StatusBadRequest, helpers.Envolope{"msg": "The request couldn't be read."})
		return req, nil, false
	}
	if err := docAIRun.CheckRateLimit(ctx, userInfo.UserDgraphInfo.Uuid); err != nil {
		helpers.WriteJSON(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "You've asked AI a lot in the last minute. Try again in a moment."})
		return req, nil, false
	}
	return req, &userInfo, true
}

func writeDocAIError(w http.ResponseWriter, ctx context.Context, err error) {
	var de *docAIError
	if errors.As(err, &de) {
		helpers.WriteJSON(w, de.status, helpers.Envolope{"msg": de.msg})
		return
	}
	helpers.LogErrorWithContext(ctx, "controllers/DocAI prepare failed: %v", err)
	helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Doc AI couldn't start. Try again in a moment."})
}

// breakerRefusal answers a closed circuit or a full queue for the model.
func breakerRefusal(w http.ResponseWriter, err error) {
	status := http.StatusServiceUnavailable
	if !errors.Is(err, ai.ErrCircuitOpen) {
		status = http.StatusTooManyRequests
	}
	helpers.WriteJSON(w, status, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
}

// DocAIModel handles GET /doc/ai/model: the model the person's doc AI runs on,
// for the bar to name before anything is sent.
func DocAIModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userInfo := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if !docAIRun.Enabled() {
		helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": map[string]any{"enabled": false}})
		return
	}
	_, _, _, model := docAIRun.Resolve(ctx, userInfo.UserDgraphInfo.Uuid)
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"msg": "ok", "data": map[string]any{"enabled": true, "model": model}})
}

// DocAIComplete handles POST /ai/doc/complete: one answer, in one piece.
func DocAIComplete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, userInfo, ok := gateDocAI(w, r, ctx)
	if !ok {
		return
	}
	ctx, p, err := prepareDocAI(ctx, userInfo, req)
	if err != nil {
		writeDocAIError(w, ctx, err)
		return
	}
	if err := p.cb.Allow(); err != nil {
		breakerRefusal(w, err)
		return
	}
	if len(p.parts) > 0 {
		partials, err := business.SummariseDocParts(ctx, p.llm, p.in.DocTitle, p.parts, p.limits, p.budget, nil)
		if err != nil {
			p.cb.RecordResult(err)
			helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
			return
		}
		p.in.DocMarkdown, p.in.Parts = p.limits.TruncateForPrompt(ctx, partials, p.budget), true
	}
	result, err := ai.ChatWithRescue(ctx, p.llm, docMessages(p.in), p.opts)
	if err != nil {
		p.cb.RecordResult(err)
		helpers.LogErrorWithContext(ctx, "controllers/DocAIComplete AI completion failed: %+v", err)
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": ai.FriendlyProviderError(err)})
		return
	}
	p.cb.RecordSuccess()
	clean := business.CleanDocAnswer(result, p.source)
	if clean == "" {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "The model sent back nothing. Try again, or pick another model."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{
		"msg": "ok",
		"data": DocAIResponse{
			Result: clean, Action: p.action.Name, Provider: p.model.Provider,
			Model: p.model, Notice: docAINotice(ctx, p),
		},
	})
}

func docMessages(in business.DocAIInput) []ai.ChatMessage {
	return []ai.ChatMessage{
		{Role: "system", Content: business.DocSystemPrompt(in)},
		{Role: "user", Content: business.DocUserContent(in)},
	}
}

// docAINotice is what the bar says about how much was read, in a doc's words
// (the service's own notice speaks of a conversation).
func docAINotice(ctx context.Context, p *preparedDocAI) string {
	n := ai.TakeContextNotice(ctx)
	switch {
	case len(n.Degraded) > 0:
		return n.Message()
	case p.cut:
		return "This doc is very long, so the summary covers its first part."
	case p.trimmed || n.Trimmed || n.Rescued:
		if p.in.Text != "" {
			return "The selection is longer than the model can read at once, so this answer used its first part."
		}
		return "The doc is longer than the model can read at once, so this answer used its first part."
	}
	return ""
}

// keepTail is the end of s that fits budget tokens, from the start of a line
// where one is near: the text nearest the caret. It bounds the text around
// what an action works on (read for tone, and for where to carry on from), not
// the text itself, which is shortened visibly. Pure.
func keepTail(limits ai.ModelLimits, s string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if limits.EstimateTokens(s) <= budget {
		return s
	}
	r := []rune(s)
	keep := int(float64(budget) * limits.CharsPerToken())
	if keep <= 0 {
		return ""
	}
	if keep >= len(r) {
		return s
	}
	tail := string(r[len(r)-keep:])
	if i := strings.Index(tail, "\n"); i >= 0 && i < len(tail)/4 {
		tail = tail[i+1:]
	}
	return strings.TrimLeft(tail, " \n")
}

// sseWriter writes the stream's frames and pings, one at a time.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseWriter) send(v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeSSE(s.w, v)
	s.flusher.Flush()
}

func (s *sseWriter) ping() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write([]byte(": ping\n\n"))
	s.flusher.Flush()
}

// DocAICompleteStream handles POST /ai/doc/complete/stream.
func DocAICompleteStream(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), ai.StreamTimeout())
	defer cancel()
	req, userInfo, ok := gateDocAI(w, r, ctx)
	if !ok {
		return
	}
	ctx, p, err := prepareDocAI(ctx, userInfo, req)
	if err != nil {
		writeDocAIError(w, ctx, err)
		return
	}
	if err := p.cb.Allow(); err != nil {
		breakerRefusal(w, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		helpers.WriteJSON(w, http.StatusInternalServerError, helpers.Envolope{"msg": "Streaming isn't available here."})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	out := &sseWriter{w: w, flusher: flusher}

	// The model is named before anything else is said.
	out.send(map[string]any{"model": p.model})

	// Pings until the handler returns, and the handler waits for the last one:
	// nothing is written to the response after it has ended.
	stop, pinged := make(chan struct{}), make(chan struct{})
	every := docAIPingEvery
	helpers.GoSafeLoop("ai.doc-stream.ping", func() {
		defer close(pinged)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				out.ping()
			}
		}
	})
	defer func() {
		close(stop)
		<-pinged
	}()

	if len(p.parts) > 0 {
		out.send(map[string]any{"progress": map[string]any{"step": "reading", "done": 0, "of": len(p.parts)}})
		partials, err := business.SummariseDocParts(ctx, p.llm, p.in.DocTitle, p.parts, p.limits, p.budget, func(done, of int) {
			out.send(map[string]any{"progress": map[string]any{"step": "reading", "done": done, "of": of}})
		})
		if err != nil {
			p.cb.RecordResult(err)
			out.send(map[string]string{"error": ai.FriendlyProviderError(err)})
			return
		}
		p.in.DocMarkdown, p.in.Parts = p.limits.TruncateForPrompt(ctx, partials, p.budget), true
		out.send(map[string]any{"progress": map[string]any{"step": "writing", "done": len(p.parts), "of": len(p.parts)}})
	}

	chunks, err := ai.ChatStreamWithRescue(ctx, p.llm, docMessages(p.in), p.opts)
	if err != nil {
		p.cb.RecordResult(err)
		out.send(map[string]string{"error": ai.FriendlyProviderError(err)})
		return
	}
	think := &business.StreamThinkFilter{}
	pre := &business.DocPreambleFilter{Input: p.source}
	var raw, sent strings.Builder
	emit := func(s string) {
		if s == "" {
			return
		}
		sent.WriteString(s)
		out.send(map[string]string{"content": s})
	}
	finish := func() {
		emit(pre.Feed(think.Flush()))
		emit(pre.Flush())
		clean := business.CleanDocAnswer(raw.String(), p.source)
		if clean == "" {
			out.send(map[string]string{"error": "The model sent back nothing. Try again, or pick another model."})
			return
		}
		if clean != strings.TrimSpace(sent.String()) {
			out.send(map[string]string{"replace": clean})
		}
		if notice := docAINotice(ctx, p); notice != "" {
			out.send(map[string]string{"notice": notice})
		}
		out.send(map[string]bool{"done": true})
	}
	for chunk := range chunks {
		if chunk.Error != nil {
			p.cb.RecordResult(chunk.Error)
			out.send(map[string]string{"error": ai.FriendlyProviderError(chunk.Error)})
			return
		}
		if chunk.Content != "" {
			raw.WriteString(chunk.Content)
			emit(pre.Feed(think.Feed(chunk.Content)))
		}
		if chunk.Done {
			p.cb.RecordSuccess()
			finish()
			return
		}
	}
	if ctx.Err() != nil {
		out.send(map[string]string{"error": "The model took too long to answer. Try again, or ask for less."})
		return
	}
	p.cb.RecordSuccess()
	finish()
}
