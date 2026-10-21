// Package taskread is the one filter on reading tasks
// (design/projects-vs-asana-jira/WA2_PLAN.md): which restricted tasks the
// reader of a request may not see, applied wherever tasks are read.
//
// business/WorkAccess decides who may read a restricted task (its project's
// admins, its assignee, its creator and the people granted on it) and plugs
// that decision in as Decide. Every read of tasks asks From for the reader of
// its context, or For for one recipient (a webhook's subscriber, a notice's
// reader), and keeps what that reader may see:
//
//   - in a graph query, Cond and AndCond leave hidden tasks out of lists and
//     counts alike, so pages and counts stay exact;
//   - in a search, IDs are the entries to leave out;
//   - in an answer already read, Keep takes hidden tasks out of every list of
//     tasks it holds and turns a single one (a parent, a comment's task) into
//     a stub that says only that there is a task the reader can't see.
//
// A context with no reader hides every restricted task: a read that can't say
// whose it is fails closed. WithSystem marks a read with no audience (a
// sweeper, an import), which hides nothing; helpers' taskReadGuard test names
// the places allowed to. The guard also holds every graph read of tasks to
// this package.
package taskread

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	restrictionModel "github.com/akashc777/OneCamp/models/postgres/TaskRestriction"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Decide is the tasks, by lower-case uuid, that reader (a user's uuid; "" for
// nobody) may not see: true. A restricted task they may see may be named
// false, and then carries the lock on what they read (DgraphTask.Restricted,
// WA2b). business/WorkAccess sets it as it loads. Until it does
// (a program or a test that doesn't link it), every restricted task is hidden
// from everyone: nothing that can't say who may see one ever shows one.
var Decide func(ctx context.Context, reader string) (map[string]bool, error)

// everyRestricted is every restricted task, by uuid: what is hidden while
// nothing decides (Decide unset). A seam for tests.
var everyRestricted = func(ctx context.Context) (map[string]bool, error) {
	rows, err := restrictionModel.Candidates(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.TaskID.String()] = true
	}
	return out, nil
}

// Filter is the restricted tasks one reader may not see, and those they may
// (which carry the lock). The zero Filter hides nothing. Safe for concurrent
// use: it is never changed once made.
type Filter struct {
	hidden map[string]bool
	// shown are the restricted tasks the reader may see.
	shown map[string]bool
}

type ctxKey int

const (
	holderKey ctxKey = iota
	systemKey
	nobodyKey
	actingKey
)

// holder keeps a request's filters, each worked out on its first read of
// tasks: by reader, since a request learns its reader as it goes (the
// signed-in person is known only after the first reads).
type holder struct {
	mu   sync.Mutex
	byID map[string]*decision
}

type decision struct {
	once sync.Once
	f    Filter
	err  error
}

func (h *holder) decision(reader string) *decision {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byID == nil {
		h.byID = map[string]*decision{}
	}
	d, ok := h.byID[reader]
	if !ok {
		d = &decision{}
		h.byID[reader] = d
	}
	return d
}

// Install gives ctx a place to keep its reader's filter, so a request works it
// out once however many reads it makes. The first middleware calls it.
func Install(ctx context.Context) context.Context {
	return context.WithValue(ctx, holderKey, &holder{})
}

// WithSystem marks reads through ctx as having no audience: a sweeper, an
// import, a copy an admin makes. They see every task. Never for anything a
// person or an agent will read; the guard lists where it may be called.
func WithSystem(ctx context.Context) context.Context {
	return context.WithValue(ctx, systemKey, true)
}

// AsNobody makes reads through ctx those of nobody in particular: what they
// make will be read by anyone (a project update everyone in the project
// reads, a template), so they leave every restricted task out, whoever
// gathers them.
func AsNobody(ctx context.Context) context.Context {
	return context.WithValue(ctx, nobodyKey, true)
}

// ActingFor makes reads through ctx those of userUUID, a user's uuid, whoever
// is signed in on ctx: an assistant's tool, or an agent's step, acting for the
// person it names. It answers with what that person may see, and through a
// token still a member's at most ("" is nobody, who sees no restricted task).
func ActingFor(ctx context.Context, userUUID string) context.Context {
	return context.WithValue(ctx, actingKey, strings.ToLower(strings.TrimSpace(userUUID)))
}

// ReaderOf is whose reads ctx carries: nobody's after AsNobody, else the
// person ActingFor names, else the request's signed-in person (a token's call
// is its owner's, as a member), else "".
func ReaderOf(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if n, _ := ctx.Value(nobodyKey).(bool); n {
		return ""
	}
	if acting, ok := ctx.Value(actingKey).(string); ok {
		return acting
	}
	switch u := ctx.Value(helpers.UserInfoContextKey).(type) {
	case userModels.UserInfo:
		return idOf(u)
	case *userModels.UserInfo:
		if u != nil {
			return idOf(*u)
		}
	}
	return ""
}

func idOf(u userModels.UserInfo) string {
	if u.UserPostgresInfo.Id != uuid.Nil {
		return u.UserPostgresInfo.Id.String()
	}
	return strings.ToLower(u.UserDgraphInfo.Uuid)
}

// System is whether ctx was marked WithSystem.
func System(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	s, _ := ctx.Value(systemKey).(bool)
	return s
}

// From is the filter for the reader of ctx (ReaderOf): worked out once per
// request and reader where Install left a place for it. With no reader it
// hides every restricted task; WithSystem hides nothing. An error is the
// decision failing, and the read should fail with it.
func From(ctx context.Context) (Filter, error) {
	if System(ctx) {
		return Filter{}, nil
	}
	reader := ReaderOf(ctx)
	if h, ok := ctx.Value(holderKey).(*holder); ok && h != nil {
		d := h.decision(reader)
		d.once.Do(func() { d.f, d.err = decide(ctx, reader) })
		return d.f, d.err
	}
	return decide(ctx, reader)
}

// For is the filter for one person by user uuid, whoever is reading ctx: what
// a notice, an email, a webhook delivery or a live update may show them.
func For(ctx context.Context, userUUID string) (Filter, error) {
	return decide(ctx, strings.ToLower(strings.TrimSpace(userUUID)))
}

func decide(ctx context.Context, reader string) (Filter, error) {
	decider := Decide
	if decider == nil {
		decider = func(ctx context.Context, _ string) (map[string]bool, error) { return everyRestricted(ctx) }
	}
	hidden, err := decider(ctx, reader)
	if err != nil {
		return Filter{}, err
	}
	return Of(hidden), nil
}

// Of is the filter that hides the tasks named true, and marks those named
// false restricted (any case; anything that isn't a uuid names no task and is
// dropped). For WorkAccess, and for tests.
func Of(taskUUIDs map[string]bool) Filter {
	if len(taskUUIDs) == 0 {
		return Filter{}
	}
	var f Filter
	for id, hide := range taskUUIDs {
		p, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil {
			continue
		}
		into := &f.shown
		if hide {
			into = &f.hidden
		}
		if *into == nil {
			*into = map[string]bool{}
		}
		(*into)[p.String()] = true
	}
	return f
}

// Restricted is whether the task is a restricted one the reader may see.
func (f Filter) Restricted(taskUUID string) bool {
	return len(f.shown) > 0 && f.shown[strings.ToLower(taskUUID)]
}

// idle is whether Keep has nothing to do: nothing hidden, nothing to mark.
func (f Filter) idle() bool { return len(f.hidden) == 0 && len(f.shown) == 0 }

// Empty is whether the filter hides nothing: the case almost every read is
// in, and costs nothing.
func (f Filter) Empty() bool { return len(f.hidden) == 0 }

// Hides is whether the reader may not see the task.
func (f Filter) Hides(taskUUID string) bool {
	return len(f.hidden) > 0 && f.hidden[strings.ToLower(taskUUID)]
}

// IDs are the hidden tasks' uuids, sorted: for a search's must_not.
func (f Filter) IDs() []string {
	out := make([]string, 0, len(f.hidden))
	for id := range f.hidden {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Cond is a graph filter condition that leaves the hidden tasks out of a
// block of tasks, or "" when nothing is hidden. Every id is a parsed uuid (Of),
// so nothing from outside reaches the query.
func (f Filter) Cond() string {
	if len(f.hidden) == 0 {
		return ""
	}
	list, _ := json.Marshal(f.IDs())
	return "NOT eq(task_uuid, " + string(list) + ")"
}

// AndCond joins Cond onto a block's own condition: "a AND NOT eq(…)", Cond
// alone when there is none, cond unchanged when nothing is hidden.
func (f Filter) AndCond(cond string) string {
	c := f.Cond()
	switch {
	case c == "":
		return cond
	case strings.TrimSpace(cond) == "":
		return c
	}
	return "(" + cond + ") AND " + c
}

// Directive is Cond as a block's whole @filter, or "" when nothing is hidden.
func (f Filter) Directive() string {
	if c := f.Cond(); c != "" {
		return "@filter(" + c + ")"
	}
	return ""
}

// Stub is what a reader who may not see a task is shown of it where a link
// to it must stay (a parent, a dependency): its id, and that it is hidden.
func Stub(t *dgraphStruct.DgraphTask) *dgraphStruct.DgraphTask {
	return &dgraphStruct.DgraphTask{
		Uuid:           t.Uuid,
		Hidden:         true,
		DependencyKind: t.DependencyKind,
		DependencyLag:  t.DependencyLag,
	}
}

// Keep takes what the reader may not see out of an answer already read: v is
// a pointer to it. A hidden task leaves every list of tasks it is in (a
// project's or a person's tasks, sub-tasks, linked tasks), except the lists
// of dependencies, where it stays as a Stub so the link is kept and nothing
// else; a single hidden task (a parent, a comment's or a mention's task)
// becomes a Stub. It walks only the parts of v that can hold a task.
func (f Filter) Keep(v any) {
	if f.idle() || v == nil {
		return
	}
	rv := reflect.ValueOf(v)
	// A pointer to a list of tasks is a list like any other: its hidden
	// tasks leave it.
	if rv.Kind() == reflect.Pointer && !rv.IsNil() && rv.Elem().Kind() == reflect.Slice {
		f.walkField(rv.Elem(), "")
		return
	}
	f.walk(rv)
}

var (
	taskType    = reflect.TypeOf(dgraphStruct.DgraphTask{})
	taskPtrType = reflect.TypeOf(&dgraphStruct.DgraphTask{})
	holdsCache  sync.Map // reflect.Type -> bool
	fieldsCache sync.Map // reflect.Type (a struct) -> []taskField
)

// taskField is a struct's field that can hold a task: its index and name.
type taskField struct {
	index int
	name  string
}

// fieldsOf is the fields of struct type t that can hold a task, worked out
// once per type: a walk visits only these, and a task has only a few among
// its many (a board of 1,500 tasks walked every field of each in 78 ms).
func fieldsOf(t reflect.Type) []taskField {
	if v, ok := fieldsCache.Load(t); ok {
		return v.([]taskField)
	}
	var out []taskField
	for i := 0; i < t.NumField(); i++ {
		if field := t.Field(i); field.IsExported() && holds(field.Type) {
			out = append(out, taskField{index: i, name: field.Name})
		}
	}
	fieldsCache.Store(t, out)
	return out
}

// stubbedLists are the lists of tasks whose hidden members stay as stubs: a
// task waits on, or holds up, one the reader can't see.
var stubbedLists = map[string]bool{"BlockedBy": true, "Blocks": true}

// holds is whether a value of type t can contain a task.
func holds(t reflect.Type) bool {
	if v, ok := holdsCache.Load(t); ok {
		return v.(bool)
	}
	holdsCache.Store(t, false) // a type that holds itself stops here
	r := holdsWalk(t)
	holdsCache.Store(t, r)
	return r
}

func holdsWalk(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		if t.Elem() == taskType {
			return true
		}
		return holds(t.Elem())
	case reflect.Map:
		return holds(t.Elem())
	case reflect.Struct:
		if t == taskType {
			return true
		}
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() && holds(t.Field(i).Type) {
				return true
			}
		}
	case reflect.Interface:
		return true
	}
	return false
}

func (f Filter) walk(v reflect.Value) {
	switch v.Kind() {
	case reflect.Interface:
		if !v.IsNil() {
			f.walk(v.Elem())
		}
	case reflect.Pointer:
		if v.IsNil() || !holds(v.Type().Elem()) {
			return
		}
		f.walk(v.Elem())
	case reflect.Struct:
		t := v.Type()
		if t == taskType {
			f.walkTask(v)
			return
		}
		for _, field := range fieldsOf(t) {
			f.walkField(v.Field(field.index), field.name)
		}
	case reflect.Slice, reflect.Array:
		if !holds(v.Type()) {
			return
		}
		for i := 0; i < v.Len(); i++ {
			f.walk(v.Index(i))
		}
	case reflect.Map:
		if v.IsNil() || !holds(v.Type().Elem()) {
			return
		}
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if e.Kind() == reflect.Pointer || e.Kind() == reflect.Interface || e.Kind() == reflect.Slice || e.Kind() == reflect.Map {
				f.walk(e)
			}
		}
	}
}

// walkTask marks a restricted task the reader may see, and applies the
// filter inside the task's own lists and links.
func (f Filter) walkTask(v reflect.Value) {
	if len(f.shown) > 0 && v.CanSet() && f.Restricted(v.Field(taskUUIDField).String()) {
		v.Field(taskRestrictedField).SetBool(true)
	}
	for _, field := range fieldsOf(v.Type()) {
		f.walkField(v.Field(field.index), field.name)
	}
}

// walkField filters one field that can hold tasks: a list of tasks loses its
// hidden ones (or stubs them, for dependencies); a single task is stubbed.
func (f Filter) walkField(fv reflect.Value, name string) {
	if !fv.CanSet() {
		f.walk(fv)
		return
	}
	switch {
	case fv.Type() == taskPtrType:
		if !fv.IsNil() {
			task := fv.Interface().(*dgraphStruct.DgraphTask)
			if f.Hides(task.Uuid) {
				fv.Set(reflect.ValueOf(Stub(task)))
				return
			}
			f.walk(fv)
		}
	case fv.Kind() == reflect.Slice && fv.Type().Elem() == taskPtrType:
		if fv.IsNil() {
			return
		}
		// Most lists hold no hidden task: walked where they are.
		if !f.hidesAny(fv) {
			for i := 0; i < fv.Len(); i++ {
				if e := fv.Index(i); !e.IsNil() {
					f.walk(e)
				}
			}
			return
		}
		keep := reflect.MakeSlice(fv.Type(), 0, fv.Len())
		for i := 0; i < fv.Len(); i++ {
			e := fv.Index(i)
			if e.IsNil() {
				continue
			}
			task := e.Interface().(*dgraphStruct.DgraphTask)
			if f.Hides(task.Uuid) {
				if stubbedLists[name] {
					keep = reflect.Append(keep, reflect.ValueOf(Stub(task)))
				}
				continue
			}
			f.walk(e)
			keep = reflect.Append(keep, e)
		}
		fv.Set(keep)
	case fv.Kind() == reflect.Slice && fv.Type().Elem() == taskType:
		if fv.IsNil() {
			return
		}
		keep := reflect.MakeSlice(fv.Type(), 0, fv.Len())
		for i := 0; i < fv.Len(); i++ {
			e := fv.Index(i)
			if f.Hides(e.Field(taskUUIDField).String()) {
				if stubbedLists[name] {
					keep = reflect.Append(keep, reflect.ValueOf(*Stub(e.Addr().Interface().(*dgraphStruct.DgraphTask))))
				}
				continue
			}
			f.walk(e.Addr())
			keep = reflect.Append(keep, e)
		}
		fv.Set(keep)
	case fv.Type() == taskType:
		if f.Hides(fv.Field(taskUUIDField).String()) {
			fv.Set(reflect.ValueOf(*Stub(fv.Addr().Interface().(*dgraphStruct.DgraphTask))))
			return
		}
		f.walkTask(fv)
	default:
		f.walk(fv)
	}
}

// hidesAny is whether the filter hides any task of list, a []*DgraphTask.
func (f Filter) hidesAny(list reflect.Value) bool {
	for i := 0; i < list.Len(); i++ {
		if e := list.Index(i); !e.IsNil() && f.Hides(e.Elem().Field(taskUUIDField).String()) {
			return true
		}
	}
	return false
}

// taskUUIDField is the index of DgraphTask.Uuid.
var taskUUIDField = func() int {
	f, ok := taskType.FieldByName("Uuid")
	if !ok {
		panic("taskread: DgraphTask has no Uuid")
	}
	return f.Index[0]
}()

// taskRestrictedField is the index of DgraphTask.Restricted.
var taskRestrictedField = func() int {
	f, ok := taskType.FieldByName("Restricted")
	if !ok || f.Type.Kind() != reflect.Bool {
		panic("taskread: DgraphTask has no Restricted flag")
	}
	return f.Index[0]
}()

// KeepTasks is Keep for a list of tasks read on their own (a query's whole
// answer): the hidden ones leave it.
func (f Filter) KeepTasks(tasks []*dgraphStruct.DgraphTask) []*dgraphStruct.DgraphTask {
	if f.idle() {
		return tasks
	}
	out := tasks[:0:0]
	for _, t := range tasks {
		if t == nil || f.Hides(t.Uuid) {
			continue
		}
		f.Keep(t)
		out = append(out, t)
	}
	return out
}

// KeepIn is Keep with the filter of ctx's reader (From): what a graph model
// calls on every answer it decodes, so no read of tasks through it shows the
// reader what they may not see. An error is the filter's, and the read's.
func KeepIn(ctx context.Context, v any) error {
	// Most answers a model decodes hold no task at all (a person, a project's
	// settings): nothing to decide.
	if v == nil || !anyTask(reflect.ValueOf(v)) {
		return nil
	}
	f, err := From(ctx)
	if err != nil {
		return err
	}
	f.Keep(v)
	return nil
}

// anyTask is whether v holds a task: a non-nil task, or a list with one.
func anyTask(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return false
		}
		if v.Kind() == reflect.Pointer && v.Type().Elem() == taskType {
			return true
		}
		if v.Kind() == reflect.Pointer && !holds(v.Type().Elem()) {
			return false
		}
		return anyTask(v.Elem())
	case reflect.Struct:
		if v.Type() == taskType {
			return true
		}
		for _, field := range fieldsOf(v.Type()) {
			if anyTask(v.Field(field.index)) {
				return true
			}
		}
	case reflect.Slice, reflect.Array:
		if !holds(v.Type()) {
			return false
		}
		for i := 0; i < v.Len(); i++ {
			if anyTask(v.Index(i)) {
				return true
			}
		}
	case reflect.Map:
		if v.IsNil() || !holds(v.Type().Elem()) {
			return false
		}
		for _, k := range v.MapKeys() {
			if anyTask(v.MapIndex(k)) {
				return true
			}
		}
	}
	return false
}

// Settle is KeepIn on a graph model's named result, deferred at the top of the
// model's read: `defer taskread.Settle(ctx, &result, &err)`. It leaves an
// error alone and, on an answer, keeps only what the reader may see, or fails
// the read with the filter's error.
func Settle(ctx context.Context, result any, err *error) {
	if err == nil || *err != nil {
		return
	}
	if e := KeepIn(ctx, result); e != nil {
		*err = e
	}
}

// Mark is where a query leaves room for the hidden tasks: inside a block's
// @filter, after a condition of its own ("@filter(<live> @@hidden_tasks@@)").
// Fill puts " AND NOT eq(task_uuid, […])" there, or nothing. A query sent
// with a Mark left in it fails, so a block that asks for the filter can't
// quietly go without it.
const Mark = "@@hidden_tasks@@"

// Fill fills every Mark in query with the filter.
func (f Filter) Fill(query string) string {
	and := ""
	if c := f.Cond(); c != "" {
		and = " AND " + c
	}
	return strings.ReplaceAll(query, Mark, and)
}

// And is " AND " and Cond, to put after a condition of a query's own, or ""
// when nothing is hidden.
func (f Filter) And() string {
	if c := f.Cond(); c != "" {
		return " AND " + c
	}
	return ""
}

// Nobody is the filter of a read whose result anyone may come to see: a
// template, an update read by a whole project, a public page. It hides every
// restricted task.
func Nobody(ctx context.Context) (Filter, error) {
	return decide(ctx, "")
}

// In is the hidden tasks as a graph function's argument, "eq(task_uuid,
// […])", for a block that finds them (to leave out what hangs off them), or
// "" when nothing is hidden.
func (f Filter) In() string {
	if len(f.hidden) == 0 {
		return ""
	}
	list, _ := json.Marshal(f.IDs())
	return "eq(task_uuid, " + string(list) + ")"
}

// ForReader is the filter for userUUID: the request's own (From, worked out
// once) when userUUID is ctx's reader, else theirs alone (For). For code that
// names whom it acts for, as the assistant's searches do.
func ForReader(ctx context.Context, userUUID string) (Filter, error) {
	if strings.EqualFold(strings.TrimSpace(userUUID), ReaderOf(ctx)) {
		return From(ctx)
	}
	return For(ctx, userUUID)
}

// SearchWithout wraps a search body (OpenSearch's JSON) so that no entry
// whose fields name one of the hidden tasks matches it, however its query is
// built: the query becomes the must of a bool whose must_not names them, and
// the body's other parts (size, sort, highlight, search_after) are kept. So
// a search's counts and pages leave them out, not only its answer. With
// nothing hidden, the body is unchanged. Each field must be a keyword.
func (f Filter) SearchWithout(body string, fields ...string) (string, error) {
	if len(f.hidden) == 0 || len(fields) == 0 {
		return body, nil
	}
	var parts map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &parts); err != nil {
		return "", err
	}
	q, ok := parts["query"]
	if !ok {
		q = json.RawMessage(`{"match_all": {}}`)
	}
	ids := f.IDs()
	mustNot := make([]map[string]any, 0, len(fields))
	for _, field := range fields {
		mustNot = append(mustNot, map[string]any{"terms": map[string]any{field: ids}})
	}
	wrapped, err := json.Marshal(map[string]any{"bool": map[string]any{"must": []json.RawMessage{q}, "must_not": mustNot}})
	if err != nil {
		return "", err
	}
	parts["query"] = wrapped
	out, err := json.Marshal(parts)
	return string(out), err
}
