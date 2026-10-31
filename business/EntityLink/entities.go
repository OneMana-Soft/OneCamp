package entityLinkBusiness

// Live Entity Chips, the server's half (docs programme §4 #21, built by C1):
// what a chip shows of a thing (a doc's title and icon, a task's status), for
// one reader, with that reader's access decided for every thing read. A chip
// for something the reader can't open answers access "none" and nothing else,
// so its title never reaches them (I12); one that is gone answers "gone".
//
// Each kind of thing registers its reader once (RegisterKind): docs, tasks,
// projects and people here; the calendar's events, the tables' rows and
// chat's calls register theirs from their own packages, and the web app's
// chip registry (lib/liveEntity) asks for any of them through one endpoint,
// POST /entities. The same reader is the kind's title reader for business/
// Access ReadableTitles, so a chip and every other place that names a thing
// to someone decide it by the one read. A reader that can say when its thing
// changes gives the topic to listen on (Entity.Topic); the broker asks
// business/MqttAccess again before it lets anyone subscribe to it.

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/business/Access/level"
	taskBusiness "github.com/akashc777/OneCamp/business/Task"
	taskKeys "github.com/akashc777/OneCamp/business/TaskKeys"
	taskStatusBusiness "github.com/akashc777/OneCamp/business/TaskStatus"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	domain "github.com/akashc777/OneCamp/domain/EntityLink"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

// What a reader may do with a thing, as a chip says it.
const (
	AccessOK   = "ok"
	AccessNone = "none"
	AccessGone = "gone"
)

// Ref names one thing.
type Ref struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Entity is one thing as a chip shows it to one reader. Data is the kind's
// own (its title among it, as the web app reads it), and Topic the MQTT topic
// that says when it changes; Title, Data and Topic are empty unless Access is
// "ok". Title is what ReadableTitles names the thing by.
type Entity struct {
	Kind   string         `json:"kind"`
	ID     string         `json:"id"`
	Access string         `json:"access"`
	Title  string         `json:"-"`
	Data   map[string]any `json:"data,omitempty"`
	Topic  string         `json:"topic,omitempty"`
}

// Reader reads things of one kind for a reader: an entry per id it may show
// or knows is gone. An id it leaves out reads as access "none", so a reader
// can't leak by forgetting to check.
type Reader func(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error)

// MaxRefs is how many things one read takes.
const MaxRefs = 100

var (
	kindsMu sync.RWMutex
	kinds   = map[string]Reader{}
)

// RegisterKind adds the reader for a kind of thing, from its package's init,
// and makes it the kind's title reader (business/Access RegisterTitleReader).
// A kind registered twice panics, where two packages made the same mistake.
func RegisterKind(kind string, read Reader) {
	kindsMu.Lock()
	defer kindsMu.Unlock()
	if kind == "" || read == nil {
		panic("entity link: a kind needs a name and a reader")
	}
	if _, dup := kinds[kind]; dup {
		panic(fmt.Sprintf("entity link: the kind %q is registered twice", kind))
	}
	kinds[kind] = read
	accessBusiness.RegisterTitleReader(kind, titlesOf(read))
}

// titlesOf is a kind's reader as its title reader: the title of each thing
// the reader may open, read and decided as its chip is.
func titlesOf(read Reader) accessBusiness.TitleReader {
	return func(ctx context.Context, ids []string, who accessBusiness.Subject) (map[string]string, error) {
		got, err := read(ctx, who, ids)
		if err != nil {
			return nil, err
		}
		out := make(map[string]string, len(got))
		for id, e := range got {
			if e.Access == AccessOK {
				out[id] = e.Title
			}
		}
		return out, nil
	}
}

// ErrTooMany is a read of more than MaxRefs things.
var ErrTooMany = fmt.Errorf("read at most %d things at once", MaxRefs)

// ReadEntities is each of refs as one reader may see it, in the order asked.
// A kind nobody registered reads as access "none".
func ReadEntities(ctx context.Context, who accessBusiness.Subject, refs []Ref) ([]Entity, error) {
	if len(refs) > MaxRefs {
		return nil, ErrTooMany
	}
	return ReadMany(ctx, who, refs)
}

// ReadMany is ReadEntities without its cap, for a server-side caller that
// bounds its own reads (the work graph's few hundred nodes): each kind is
// still read once, however many of it are asked.
func ReadMany(ctx context.Context, who accessBusiness.Subject, refs []Ref) ([]Entity, error) {
	byKind := map[string][]string{}
	for _, r := range refs {
		byKind[r.Kind] = append(byKind[r.Kind], r.ID)
	}
	found := map[Ref]Entity{}
	kindNames := make([]string, 0, len(byKind))
	for k := range byKind {
		kindNames = append(kindNames, k)
	}
	sort.Strings(kindNames)
	for _, kind := range kindNames {
		kindsMu.RLock()
		read := kinds[kind]
		kindsMu.RUnlock()
		if read == nil {
			continue
		}
		got, err := read(ctx, who, byKind[kind])
		if err != nil {
			return nil, err
		}
		for id, e := range got {
			e.Kind, e.ID = kind, id
			if e.Access != AccessOK {
				e.Title, e.Data, e.Topic = "", nil, ""
			}
			found[Ref{Kind: kind, ID: id}] = e
		}
	}
	out := make([]Entity, len(refs))
	for i, r := range refs {
		e, ok := found[r]
		if !ok {
			e = Entity{Kind: r.Kind, ID: r.ID, Access: AccessNone}
		}
		out[i] = e
	}
	return out, nil
}

// Where each kind reads from (seams for tests).
var (
	readDocChips     = domain.DocChips
	readTaskChips    = domain.TaskChips
	layTaskKeys      = taskKeys.Lay
	readProjectChips = domain.ProjectChips
	readPeople       = userDomain.GetPeopleByUUIDs
)

func init() {
	RegisterKind("doc", readDocs)
	RegisterKind("task", readTasks)
	RegisterKind("project", readProjects)
	RegisterKind("person", readPersons)
}

// askedAs maps each id, in lower case, to the ids as they were asked.
func askedAs(ids []string) map[string][]string {
	out := map[string][]string{}
	for _, id := range ids {
		lower := strings.ToLower(id)
		out[lower] = append(out[lower], id)
	}
	return out
}

// nodeKindKey is the key a doc's data names the node under when it isn't a
// page (docs F1): folder, file or link, domain/Doc's NodeFolder, NodeFile and
// NodeLink. A page has none. Never "doc_node_kind": that is the predicate,
// written through DgraphDoc alone.
const nodeKindKey = "node_kind"

// NodeKindOf is what a doc's data says the node is: folder, file or link,
// and "" for a page or for anything that isn't a doc.
func NodeKindOf(data map[string]any) string {
	k, _ := data[nodeKindKey].(string)
	return k
}

// readDocs reads the chips of every doc asked, then decides each doc, and each
// chip's parent, through the Access Resolver in one batch: a chip shows only
// docs the reader may open, and names a parent only when they may open that
// too (a page shared out of a private folder must not reveal the folder). It
// names the doc's project or team space only to a reader in that space, which
// the doc's own decision says (its "space" reason), so a page shared out of a
// private project doesn't reveal the project either. A chip says what the node
// is when it isn't a page, and its colour when it has one, from the same read
// (a link's address isn't a chip's: the link's own page opens it; its
// provider's family is, so the panel marks it as the docs tree does). Reading
// first keeps it to one chip read and one decision, as before.
func readDocs(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	lowers := make([]string, 0, len(asked))
	for lower := range asked {
		lowers = append(lowers, lower)
	}
	chips, err := readDocChips(ctx, lowers)
	if err != nil {
		return nil, err
	}
	res := make([]accessBusiness.Resource, 0, len(asked))
	decide := map[string]bool{}
	for _, lower := range lowers {
		decide[lower] = true
		res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: lower})
	}
	for _, d := range chips {
		if p := parentOf(d); p != "" && !decide[p] {
			decide[p] = true
			res = append(res, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: p})
		}
	}
	decisions, err := accessBusiness.ResolveAll(ctx, who, res)
	if err != nil {
		return nil, err
	}
	opens, inSpace := map[string]bool{}, map[string]bool{}
	out := map[string]Entity{}
	for r, d := range decisions {
		lower := strings.ToLower(r.UUID)
		switch {
		case d.Level >= accessBusiness.LevelView:
			opens[lower] = true
			inSpace[lower] = slices.Contains(d.Reasons, string(level.Space))
		case d.Deleted && d.Was >= accessBusiness.LevelView:
			for _, id := range asked[lower] {
				out[id] = Entity{Access: AccessGone}
			}
		}
	}
	for lower, d := range chips {
		if !opens[lower] || len(asked[lower]) == 0 {
			continue
		}
		data := map[string]any{"title": d.Title, "icon": d.Icon}
		if p := parentOf(d); p != "" && opens[p] {
			data["parent_id"] = d.Parent.Uuid
		}
		if d.SpaceKind != "" && d.SpaceUUID != "" && inSpace[lower] {
			data["space_kind"], data["space_id"] = d.SpaceKind, d.SpaceUUID
		}
		if d.NodeKind != "" {
			data[nodeKindKey] = d.NodeKind
		}
		if d.NodeKind == docDomain.NodeLink {
			data["link_family"] = docDomain.LinkFamily(d.LinkURL)
		}
		if d.Color != "" {
			data["color"] = d.Color
		}
		for _, id := range asked[lower] {
			out[id] = Entity{Access: AccessOK, Title: d.Title, Data: data, Topic: helpers.GetMqttTopicForDoc(lower)}
		}
	}
	return out, nil
}

// parentOf is a doc's parent's uuid in lower case; "" for a doc at the top.
func parentOf(d *dgraphStruct.DgraphDoc) string {
	if d == nil || d.Parent == nil {
		return ""
	}
	return strings.ToLower(d.Parent.Uuid)
}

// readTasks reads tasks with the task view's rule for who may see each
// (CanViewTask) and Work Access's for who may change it, with one read of the
// reader's roles. A task in a deleted project is gone with it.
func readTasks(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	tasks, err := readTaskChips(ctx, ids, who.UserUID)
	if err != nil {
		return nil, err
	}
	// A chip names a task by its key as the rest of the app does ("Q4-12"):
	// one read for them all; a task the reader can't see keeps none.
	layTaskKeys(ctx, tasks...)
	var projects []*dgraphStruct.DgraphProject
	for _, t := range tasks {
		if t != nil && t.Project != nil {
			projects = append(projects, t.Project)
		}
	}
	standings, err := workAccess.OfMany(ctx, who.UserUUID, projects)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, t := range tasks {
		if t == nil || !taskBusiness.CanViewTask(t, who.UserUID) {
			continue
		}
		e := Entity{Access: AccessGone}
		if !helpers.IsSoftDeleted(t.DeletedAt) && (t.Project == nil || !helpers.IsSoftDeleted(t.Project.DeletedAt)) {
			canEdit := false
			if t.Project != nil {
				canEdit = standings[strings.ToLower(t.Project.Uuid)].CanOnTask(workAccess.NeedEditTask, workAccess.FactsOf(t, who.UserUID))
			}
			e = Entity{Access: AccessOK, Title: t.Name, Data: taskData(t, canEdit), Topic: projectTopicFor(t.Project)}
		}
		for _, id := range asked[strings.ToLower(t.Uuid)] {
			out[id] = e
		}
	}
	return out, nil
}

// projectTopicFor is a project's topic for a reader in it, who may listen on
// it; "" for anyone else.
func projectTopicFor(p *dgraphStruct.DgraphProject) string {
	if !p.ReaderIsIn() || p.Uuid == "" {
		return ""
	}
	return helpers.GetMqttTopicForProjectMessage(p.Uuid)
}

// taskData is what a task's chip shows; canEdit is whether its reader may
// change it (tick it done).
func taskData(t *dgraphStruct.DgraphTask, canEdit bool) map[string]any {
	status := taskStatusBusiness.Of(t)
	data := map[string]any{
		"name":        t.Name,
		"status":      status.Category,
		"status_name": status.Display(),
		"done":        status.Category == dgraphStruct.TASK_STATUS_DONE,
		"can_edit":    canEdit,
	}
	if t.DueDate != nil && !t.DueDate.IsZero() {
		data["due"] = t.DueDate.Format("2006-01-02")
	}
	if a := t.Assignee; a != nil && a.Uuid != "" {
		data["assignee"] = map[string]any{"uuid": a.Uuid, "name": a.DisplayName(), "avatar_key": a.ProfileKey}
	}
	if p := t.Project; p != nil && p.Uuid != "" {
		data["project_id"], data["project_name"] = p.Uuid, p.Name
	}
	if t.Key != "" {
		data["key"] = t.Key
	}
	return data
}

// readProjects reads each project for the people in it only; can_edit is
// whether the reader makes and changes its tasks (Work Access).
func readProjects(ctx context.Context, who accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	projects, err := readProjectChips(ctx, ids, who.UserUID)
	if err != nil {
		return nil, err
	}
	standings, err := workAccess.OfMany(ctx, who.UserUUID, projects)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, p := range projects {
		if !p.ReaderIsIn() || p.Uuid == "" {
			continue
		}
		e := Entity{Access: AccessGone}
		if !helpers.IsSoftDeleted(p.DeletedAt) {
			canEdit := standings[strings.ToLower(p.Uuid)].Can(workAccess.NeedEditTask)
			e = Entity{Access: AccessOK, Title: p.Name, Data: map[string]any{"name": p.Name, "can_edit": canEdit}, Topic: projectTopicFor(p)}
		}
		for _, id := range asked[strings.ToLower(p.Uuid)] {
			out[id] = e
		}
	}
	return out, nil
}

// readPersons reads people as every member may see them: their name, picture
// and whether they're an agent or a bot.
func readPersons(ctx context.Context, _ accessBusiness.Subject, ids []string) (map[string]Entity, error) {
	asked := askedAs(ids)
	people, err := readPeople(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := map[string]Entity{}
	for _, p := range people {
		if p == nil || p.Uuid == "" {
			continue
		}
		name := p.DisplayName()
		data := map[string]any{"name": name, "avatar_key": p.ProfileKey, "is_bot": p.IsBot}
		for _, id := range asked[strings.ToLower(p.Uuid)] {
			out[id] = Entity{Access: AccessOK, Title: name, Data: data}
		}
	}
	return out, nil
}
