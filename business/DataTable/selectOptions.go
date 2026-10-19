package business

// Select options and the cells holding them.
//
// A select or multi-select cell holds its option's label. Renaming an option
// changed only the field's config, so every cell that had it kept the old
// label: it lost its colour, the board put its card under "No value", and a
// filter on the new label missed it (TAB-BUG-3). Removing an option left its
// cells holding a label nothing offered.
//
// Now each option keeps an id ({"id", "label", "color"}) through renames and
// moves, and saving a field's options rewrites its cells in the same
// transaction: a renamed option's label everywhere it's used, a removed one
// taken out. Options are matched by id, never by place, since a drag in the
// options list moves them without renaming any.
//
// An option stored without an id (every option saved before ids) is known by
// its label as "label:<label>", here and on the web, which works it out the
// same way; labels are unique in a field. Saving the field stores the ids,
// which never change again. Working the id out from what's stored makes the
// repair idempotent: it needs no migration and no pass over old fields, and
// gives the same id however often it's done.

import (
	"strings"

	fieldTypes "github.com/akashc777/OneCamp/business/FieldTypes"
	"github.com/akashc777/OneCamp/helpers"
)

// hasOptions is whether a field of type typ keeps its cells as option labels.
func hasOptions(typ string) bool {
	k, ok := fieldTypes.Kinds.Lookup(typ)
	return ok && k.HasOptions
}

// removedOptionsKey is where a field save names, by id, the options it takes
// out. It's read from the config sent, and never stored.
const removedOptionsKey = "removed_options"

// removedOptions is the option ids a field config sent names as taken out.
func removedOptions(cfg map[string]interface{}) map[string]bool {
	out := map[string]bool{}
	list, _ := cfg[removedOptionsKey].([]interface{})
	for _, it := range list {
		if s, ok := it.(string); ok && strings.TrimSpace(s) != "" {
			out[strings.TrimSpace(s)] = true
		}
	}
	return out
}

// planOptions gives each option a field is saved with (cfg's, rewritten in
// place) an id, against the options it had (stored; none for a new field):
//  1. the id it was sent with, where a stored option without one is known by
//     storedOptionID;
//  2. else the id of the stored option with the same label, so a caller
//     sending labels alone keeps every option, and every cell;
//  3. else storedOptionID of its label when no option has it, or a new one.
//
// An option is never known by its place: a new label where a stored option
// was is a new option. A list that leaves options out (they're kept) moves
// the rest up, and a page's list read before an option was added (another
// tab) ends where the other's begins, so a place read as a rename gave cells
// another option's label. The web before ids renames by the id it read, once
// a save has stored the field's ids; the first rename of a field saved
// before then leaves its cells as they are, as v2.72.2 did.
//
// A stored option the save doesn't list is kept, after those it lists: a save
// from a copy read before an option was added (another tab, or a menu opened
// on the field as it was) took it out and emptied every cell holding it. Only
// an option whose id is in removed is taken out.
//
// A whole list (whole: a save of a whole field, FieldPatch.whole, as the web
// released before ids sends every one) is read as that web meant it, as
// v2.72.2 read it: the options it lists are the field's, so one it leaves out
// is taken out of the field, and the cells holding it keep their label, as
// they did then.
//
// It returns how the field's cells change: each stored option renamed, by its
// old label, to its new label, and each taken out by removed to "". One taken
// out whose label another option has changes nothing.
func planOptions(stored []map[string]interface{}, cfg map[string]interface{}, removed map[string]bool, whole bool) map[string]string {
	storedLabel := map[string]string{} // by id
	storedOpt := map[string]map[string]interface{}{}
	storedByLabel := map[string]string{}
	storedAt := make([]string, len(stored)) // each stored option's id, by place
	for i, o := range stored {
		id, label := fieldTypes.OptionID(o), fieldTypes.OptionLabel(o)
		if id == "" {
			id = fieldTypes.StoredOptionID(label)
		}
		if _, seen := storedLabel[id]; seen {
			continue // a label stored twice without ids is one option
		}
		storedLabel[id], storedOpt[id] = label, o
		storedAt[i] = id
		if _, ok := storedByLabel[label]; !ok {
			storedByLabel[label] = id
		}
	}
	opts := fieldTypes.Options(cfg)
	ids := make([]string, len(opts))
	taken := map[string]bool{}
	offered := map[string]bool{}
	for i, o := range opts {
		offered[fieldTypes.OptionLabel(o)] = true
		if id := fieldTypes.OptionID(o); id != "" && !taken[id] {
			ids[i], taken[id] = id, true
		}
	}
	for i, o := range opts {
		if sid, ok := storedByLabel[fieldTypes.OptionLabel(o)]; ok && fieldTypes.OptionID(o) == "" && !taken[sid] {
			ids[i], taken[sid] = sid, true
		}
	}
	changes := map[string]string{}
	list := make([]interface{}, 0, len(opts)+len(stored))
	for i, o := range opts {
		label := fieldTypes.OptionLabel(o)
		if ids[i] == "" {
			id := fieldTypes.StoredOptionID(label)
			if _, isStored := storedLabel[id]; isStored || taken[id] {
				id = fieldTypes.NewOptionID()
			}
			ids[i], taken[id] = id, true
		}
		o["id"] = ids[i]
		if was, ok := storedLabel[ids[i]]; ok && was != label {
			changes[was] = label
		}
		list = append(list, o)
	}
	// The one rule for options a save leaves out (helpers.KeptOptions, which
	// task fields follow too): kept, unless removed names them.
	keptIDs, out := helpers.KeptOptions(storedAt, func(id string) string { return id },
		func(id string) bool { return id == "" || taken[id] || offered[storedLabel[id]] }, removed)
	for _, id := range out {
		changes[storedLabel[id]] = ""
	}
	if whole {
		keptIDs = nil
	}
	for _, id := range keptIDs {
		kept := make(map[string]interface{}, len(storedOpt[id])+1)
		for k, v := range storedOpt[id] {
			kept[k] = v
		}
		kept["id"] = id
		list = append(list, kept)
	}
	if cfg["options"] != nil || len(list) > 0 {
		cfg["options"] = list
	}
	return changes
}
