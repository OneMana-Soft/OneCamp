package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// itemsWorld is a table with a relation field linking to any item, a text
// field, and a formula reading the links; and who may open which item.
type itemsWorld struct {
	links, name, title *model.Field
	fields             []*model.Field
	// opens is each reader's (by uuid) titles of the items they may open, by
	// kind, then id. People are named alike for everyone (opens[""]).
	opens map[string]map[string]map[string]string
	// failing kinds' lookups fail.
	failing map[string]bool
	// asked is each lookup made, as kind/reader.
	asked []string
}

func newItemsWorld(t *testing.T) *itemsWorld {
	table := uuid.New()
	w := &itemsWorld{opens: map[string]map[string]map[string]string{}, failing: map[string]bool{}}
	w.links = &model.Field{Id: uuid.New(), TableId: table, Name: "Linked", Type: model.FieldRelation, Config: `{"relation_target":"any"}`}
	w.name = &model.Field{Id: uuid.New(), TableId: table, Name: "Name", Type: model.FieldText, Config: `{}`}
	w.title = &model.Field{Id: uuid.New(), TableId: table, Name: "Linked titles", Type: model.FieldFormula,
		Config: fmt.Sprintf(`{"formula":"{#%s}"}`, w.links.Id)}
	w.fields = []*model.Field{w.name, w.links, w.title}
	saved := itemNamers
	itemNamers = map[string]func(context.Context, []string, *itemReader) (map[string]string, error){}
	for kind := range itemKinds {
		itemNamers[kind] = func(_ context.Context, ids []string, who *itemReader) (map[string]string, error) {
			reader := who.uuid
			if kind == itemUser {
				reader = ""
			}
			w.asked = append(w.asked, kind+"/"+reader)
			if w.failing[kind] {
				return nil, errors.New("the graph isn't answering")
			}
			out := map[string]string{}
			for _, id := range ids {
				if title, ok := w.opens[reader][kind][id]; ok {
					out[id] = title
				}
			}
			return out, nil
		}
	}
	t.Cleanup(func() { itemNamers = saved })
	return w
}

// may lets reader (a uuid; "" for people) open an item of kind, by title.
func (w *itemsWorld) may(reader, kind, id, title string) {
	if w.opens[reader] == nil {
		w.opens[reader] = map[string]map[string]string{}
	}
	if w.opens[reader][kind] == nil {
		w.opens[reader][kind] = map[string]string{}
	}
	w.opens[reader][kind][id] = title
}

// read works out a row with these stored links for ctx's reader, and gives
// back its links cell and its formula's value.
func (w *itemsWorld) read(t *testing.T, ctx context.Context, stored string) ([]itemRef, interface{}) {
	t.Helper()
	row := &model.Row{Id: uuid.New(), Values: fmt.Sprintf(`{%q: "Acme", %q: %s}`, w.name.Id, w.links.Id, stored)}
	withComputed(ctx, w.fields, []*model.Row{row})
	var cells map[string]json.RawMessage
	if err := json.Unmarshal([]byte(row.Values), &cells); err != nil {
		t.Fatalf("the row read back isn't JSON: %v", err)
	}
	var refs []itemRef
	if raw, ok := cells[w.links.Id.String()]; ok {
		if err := json.Unmarshal(raw, &refs); err != nil {
			t.Fatalf("the links cell %s: %v", raw, err)
		}
	}
	var title interface{}
	_ = json.Unmarshal(cells[w.title.Id.String()], &title)
	return refs, title
}

func TestItemLinksAreNamedForTheirReader(t *testing.T) {
	w := newItemsWorld(t)
	maya, bo := Actor{UserID: uuid.New()}, Actor{UserID: uuid.New()}
	plan, handbook, task, person := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	w.may(maya.UserID.String(), itemDoc, plan, "Restructuring plan Q4")
	w.may(maya.UserID.String(), itemDoc, handbook, "Handbook")
	w.may(bo.UserID.String(), itemDoc, handbook, "Handbook")
	w.may(maya.UserID.String(), itemTask, task, "Close the Pune office")
	w.may("", itemUser, person, "Maya Iyer")
	// As stored before: each link with a label copied when it was made, one
	// of them forged.
	stored := fmt.Sprintf(`[{"id":%q,"label":"Restructuring plan Q4","type":"doc"},{"id":%q,"label":"Forged","type":"doc"},`+
		`{"id":%q,"label":"Close the Pune office","type":"task"},{"id":%q,"label":"Maya","type":"user"}]`, plan, handbook, task, person)
	ctx := context.Background()

	t.Run("a member who can't open an item gets it hidden, without its title", func(t *testing.T) {
		refs, title := w.read(t, asViewer(ctx, bo), stored)
		want := []itemRef{{ID: plan, Type: itemDoc, Hidden: true}, {ID: handbook, Type: itemDoc, Label: "Handbook"},
			{ID: task, Type: itemTask, Hidden: true}, {ID: person, Type: itemUser, Label: "Maya Iyer"}}
		if !reflect.DeepEqual(refs, want) {
			t.Errorf("Bo reads %+v, want %+v", refs, want)
		}
		if title != "Handbook, Maya Iyer" {
			t.Errorf("Bo's formula reads %q", title)
		}
	})

	t.Run("the member who can open everything gets the current titles, never a stored label", func(t *testing.T) {
		refs, title := w.read(t, asViewer(ctx, maya), stored)
		if len(refs) != 4 || refs[1].Label != "Handbook" || refs[0].Label != "Restructuring plan Q4" || refs[2].Label != "Close the Pune office" {
			t.Errorf("Maya reads %+v", refs)
		}
		if title != "Restructuring plan Q4, Handbook, Close the Pune office, Maya Iyer" {
			t.Errorf("Maya's formula reads %q", title)
		}
	})

	// A person's id is a member's, which no guest sees; nor anyone the read
	// can't name, such as a person who left.
	t.Run("a guest is shown people's names only", func(t *testing.T) {
		left := uuid.NewString()
		withLeaver := strings.TrimSuffix(stored, "]") + fmt.Sprintf(`,{"id":%q,"type":"user"}]`, left)
		for _, gctx := range []context.Context{asGuest(ctx), ctx} {
			refs, title := w.read(t, gctx, withLeaver)
			if want := []itemRef{{Type: itemUser, Label: "Maya Iyer"}}; !reflect.DeepEqual(refs, want) {
				t.Errorf("a guest reads %+v, want %+v", refs, want)
			}
			if title != "Maya Iyer" {
				t.Errorf("a guest's formula reads %q", title)
			}
		}
	})

	t.Run("an agent asked by someone else names only what both can open", func(t *testing.T) {
		refs, _ := w.read(t, AlsoFor(asViewer(ctx, maya), bo), stored)
		if refs[0].Hidden != true || refs[0].Label != "" || refs[1].Label != "Handbook" || refs[3].Label != "Maya Iyer" {
			t.Errorf("Maya's agent asked by Bo reads %+v", refs)
		}
	})

	t.Run("items a lookup couldn't name are hidden", func(t *testing.T) {
		w.failing[itemDoc] = true
		defer delete(w.failing, itemDoc)
		refs, _ := w.read(t, asViewer(ctx, maya), stored)
		if !refs[0].Hidden || !refs[1].Hidden || refs[2].Label != "Close the Pune office" {
			t.Errorf("with docs failing Maya reads %+v", refs)
		}
	})

	t.Run("an item with no title is called untitled, not hidden", func(t *testing.T) {
		blank := uuid.NewString()
		w.may(maya.UserID.String(), itemDoc, blank, " ")
		refs, _ := w.read(t, asViewer(ctx, maya), fmt.Sprintf(`[{"id":%q,"type":"doc"}]`, blank))
		if want := []itemRef{{ID: blank, Type: itemDoc, Label: "Untitled doc"}}; !reflect.DeepEqual(refs, want) {
			t.Errorf("an untitled doc reads %+v", refs)
		}
	})

	t.Run("each kind is looked up once a read, and people once for everyone", func(t *testing.T) {
		w.asked = nil
		w.read(t, AlsoFor(asViewer(ctx, maya), bo), stored)
		counts := map[string]int{}
		for _, a := range w.asked {
			counts[strings.SplitN(a, "/", 2)[0]]++
		}
		if counts[itemDoc] != 2 || counts[itemTask] != 2 || counts[itemUser] != 1 {
			t.Errorf("an agent's read looked up %v", w.asked)
		}
	})
}

// One read names so many items at most; the rest come back hidden.
func TestOneReadNamesSoManyItems(t *testing.T) {
	w := newItemsWorld(t)
	maya := Actor{UserID: uuid.New()}
	var stored []string
	for i := 0; i <= maxReadItems; i++ {
		id := uuid.NewString()
		w.may(maya.UserID.String(), itemDoc, id, fmt.Sprint("Doc ", i))
		stored = append(stored, fmt.Sprintf(`{"id":%q,"type":"doc"}`, id))
	}
	refs, _ := w.read(t, asViewer(context.Background(), maya), "["+strings.Join(stored, ",")+"]")
	if len(refs) != maxReadItems+1 || refs[maxReadItems-1].Label != fmt.Sprint("Doc ", maxReadItems-1) || !refs[maxReadItems].Hidden {
		t.Errorf("the last two of %d links read %+v and %+v", len(refs), refs[maxReadItems-1], refs[maxReadItems])
	}
}

func TestItemLinksAreStoredAsIDs(t *testing.T) {
	doc, task := uuid.NewString(), uuid.NewString()
	anyField := &model.Field{Id: uuid.New(), Name: "Linked", Type: model.FieldRelation, Config: `{"relation_target":"any"}`}
	docs := &model.Field{Id: uuid.New(), Name: "Docs", Type: model.FieldRelation, Config: `{"relation_target":"doc"}`}
	fields := []*model.Field{anyField, docs}
	store := func(t *testing.T, field *model.Field, cell string) (string, error) {
		t.Helper()
		var v interface{}
		if err := json.Unmarshal([]byte(cell), &v); err != nil {
			t.Fatal(err)
		}
		values := map[string]interface{}{field.Id.String(): v}
		if err := keepItemIDs(fields, values); err != nil {
			return "", err
		}
		b, _ := json.Marshal(values[field.Id.String()])
		return string(b), nil
	}
	cases := []struct {
		name  string
		field *model.Field
		cell  string
		want  string // "" for refused
	}{
		{"a label written with a link isn't stored", anyField,
			fmt.Sprintf(`[{"id":%q,"label":"Anything at all","type":"doc"},{"id":%q,"type":"task","hidden":true}]`, doc, strings.ToUpper(task)),
			fmt.Sprintf(`[{"id":%q,"type":"doc"},{"id":%q,"type":"task"}]`, doc, task)},
		{"a link to one kind may leave its type out, or be an id", docs, fmt.Sprintf(`[{"id":%q},%q]`, doc, doc),
			fmt.Sprintf(`[{"id":%q,"type":"doc"}]`, doc)},
		{"a read's count of more links is passed over", anyField, fmt.Sprintf(`[{"id":%q,"type":"doc"},{"type":"more","label":"3 more"}]`, doc),
			fmt.Sprintf(`[{"id":%q,"type":"doc"}]`, doc)},
		{"a cell emptied stays empty", anyField, `[]`, `[]`},
		{"a link without an id is refused", anyField, `[{"label":"Salaries","type":"doc"}]`, ""},
		{"a link without a type to an any field is refused", anyField, fmt.Sprintf(`[{"id":%q}]`, doc), ""},
		{"a link of a kind the field doesn't link to is refused", docs, fmt.Sprintf(`[{"id":%q,"type":"task"}]`, task), ""},
		{"a link of no kind there is is refused", anyField, fmt.Sprintf(`[{"id":%q,"type":"channel"}]`, doc), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := store(t, c.field, c.cell)
			if c.want == "" {
				if err == nil || !strings.Contains(err.Error(), "by id and type") {
					t.Errorf("stored %s (%v), want it refused", got, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Errorf("stored %s (%v), want %s", got, err, c.want)
			}
		})
	}
	// Clearing a cell isn't a link.
	for _, clear := range []interface{}{nil, ""} {
		values := map[string]interface{}{anyField.Id.String(): clear}
		if err := keepItemIDs(fields, values); err != nil || values[anyField.Id.String()] != clear {
			t.Errorf("clearing with %#v: %v, %#v", clear, err, values[anyField.Id.String()])
		}
	}
}

// A link the reader can't open has no name to read, but is a link: a filter
// for empty cells doesn't take it, and nothing reads its id as a name.
func TestHiddenItemsReadAsUnnamedLinks(t *testing.T) {
	links := &model.Field{Id: uuid.New(), Name: "Linked", Type: model.FieldRelation, Config: `{"relation_target":"any"}`}
	id := uuid.NewString()
	var cell interface{}
	_ = json.Unmarshal([]byte(fmt.Sprintf(`[{"id":%q,"type":"doc","hidden":true}]`, id)), &cell)
	values := map[string]interface{}{links.Id.String(): cell}
	fields := []*model.Field{links}
	if labels := cellLabels(cell); len(labels) != 0 {
		t.Errorf("a hidden link reads as %v", labels)
	}
	for op, want := range map[FilterOp]bool{FilterEmpty: false, FilterNotEmpty: true} {
		if got := matchFilter(values, fields, Filter{Field: "Linked", Op: string(op)}); got != want {
			t.Errorf("%s on a hidden link: %v", op, got)
		}
	}
	if matchFilter(values, fields, Filter{Field: "Linked", Op: string(FilterContains), Value: id[:8]}) {
		t.Error("a hidden link's id matched a filter")
	}
}

// A link to items can't become another kind of field: its cells hold links
// each read names for its reader, and as text they'd be read as stored, by
// everyone. Changing which kind it links to is still a link.
func TestALinkToItemsKeepsItsKind(t *testing.T) {
	docs := &model.Field{Id: uuid.New(), Name: "Docs", Type: model.FieldRelation, Config: `{"relation_target":"doc"}`}
	tbl := &model.DataTable{Id: uuid.New()}
	for _, typ := range []string{model.FieldText, model.FieldSelect, model.FieldFormula} {
		_, err := computedFieldConfig(context.Background(), Actor{UserID: uuid.New()}, tbl, []*model.Field{docs}, docs.Id,
			&FieldInput{Name: "Docs", Type: typ, Config: map[string]interface{}{"formula": `"x"`}})
		if err == nil || !strings.Contains(err.Error(), "can't become another kind of field") {
			t.Errorf("a link to docs became %s: %v", typ, err)
		}
	}
	if _, err := computedFieldConfig(context.Background(), Actor{UserID: uuid.New()}, tbl, []*model.Field{docs}, docs.Id,
		&FieldInput{Name: "Docs", Type: model.FieldRelation, Config: map[string]interface{}{"relation_target": "task"}}); err != nil {
		t.Errorf("a link to docs couldn't link to tasks instead: %v", err)
	}
}

// The repair is a sweep: claimed by one worker at a time, and tried again
// the next hour when a pass fails. It was a goroutine started from main.go,
// run by every replica at once, and retried only at the next restart.
func TestTheItemLinkRepairIsASweep(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("tables.item-link-repair isn't a registered sweep")
		}
	}()
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "tables.item-link-repair", Every: time.Hour, Run: func(context.Context) error { return nil }})
}

// A refusal quotes the link it refuses briefly: a cell may be 100 KiB.
func TestARefusalQuotesBriefly(t *testing.T) {
	f := &model.Field{Id: uuid.New(), Name: "Linked", Type: model.FieldRelation, Config: `{"relation_target":"doc"}`}
	err := keepItemIDs([]*model.Field{f}, map[string]interface{}{f.Id.String(): strings.Repeat("x", 100000)})
	if err == nil || len(err.Error()) > 200 {
		t.Errorf("refusing a 100 KB cell said %d characters", len(fmt.Sprint(err)))
	}
}
