package business

// A relation field's cells when it links to OneCamp items: docs, tasks,
// boards, projects and people (relation_target "doc", "task", "board",
// "project", "user", or "any" for all of them). A row keeps [{id, type}] only;
// every read names the items for the person reading (business/DataTable
// itemLinks.go), so no title written with a link is stored or shown
// (TAB-BUG-5). A relation linking to a table's rows ("table") keeps its links
// in data_table_links instead, never in the row.

import (
	"encoding/json"
	"strings"

	model "github.com/akashc777/OneCamp/models/postgres/DataTable"
	"github.com/google/uuid"
)

// The targets a relation field can link to: a table's rows, one kind of item,
// or any item.
const (
	TargetTable = "table"
	ItemDoc     = "doc"
	ItemTask    = "task"
	ItemBoard   = "board"
	ItemProject = "project"
	ItemUser    = "user"
	ItemAny     = "any"
)

// ItemKinds is each kind of item, as a refusal names many of them.
var ItemKinds = map[string]string{
	ItemDoc: "docs", ItemTask: "tasks", ItemBoard: "boards", ItemProject: "projects", ItemUser: "people",
}

// RelationTarget is what a relation field links to: TargetTable, one kind
// of item, or ItemAny (a config naming no kind, or one nothing knows). Pure.
func RelationTarget(f *model.Field) string {
	var c struct {
		Target string `json:"relation_target"`
	}
	_ = json.Unmarshal([]byte(f.Config), &c)
	switch {
	case c.Target == TargetTable:
		return TargetTable
	case ItemKinds[c.Target] != "":
		return c.Target
	}
	return ItemAny
}

// ItemRef is a link to an item: {id, type} as stored, and as read with the
// item's title, or hidden when the reader can't open it.
type ItemRef struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type"`
	Label  string `json:"label,omitempty"`
	Hidden bool   `json:"hidden,omitempty"`
}

// ParseItems reads the links in a cell, as stored or as written: each item's
// {id, type} once, in order, and nothing else stored with it (a label copied
// when it was linked is never read). A type left out is the field's kind; a
// read's count of links past those shown ({"type": "more"}) is passed over.
// bad is the first item that isn't a link to an item the field links to, as
// it was given. Pure.
func ParseItems(raw any, target string) (refs []ItemRef, bad string) {
	list, isList := raw.([]any)
	if !isList {
		if raw == nil {
			return nil, ""
		}
		list = []any{raw}
	}
	refs = make([]ItemRef, 0, len(list))
	seen := map[string]bool{}
	for _, it := range list {
		var id, typ string
		switch v := it.(type) {
		case map[string]any:
			if v["type"] == "more" {
				continue
			}
			id, _ = v["id"].(string)
			typ, _ = v["type"].(string)
		case string:
			id = v
		}
		if typ == "" && target != ItemAny {
			typ = target
		}
		parsed, err := uuid.Parse(strings.TrimSpace(id))
		if err != nil || ItemKinds[typ] == "" || (target != ItemAny && typ != target) {
			if bad == "" {
				b, _ := json.Marshal(it)
				bad = string(b)
			}
			continue
		}
		key := typ + "/" + parsed.String()
		if !seen[key] {
			seen[key] = true
			refs = append(refs, ItemRef{ID: parsed.String(), Type: typ})
		}
	}
	return refs, bad
}
