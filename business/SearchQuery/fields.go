package business

// The fields a query may name, one registry for every area
// (design/QUERY_STACK_CONTRACT.md §4.7): the core fields this package
// registers (core.go), each area's built-in fields (chat's with: and to:,
// projects' assignee and status, …) registered from that area's init, the
// values areas add to the shared has:, is: and type: fields, and the custom
// fields of a project or a table, by name, through the scope's provider.
//
// A name means one thing in every area: registering a name or an alias twice
// panics where it happens, in an init, which fails the build's tests.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	querySpec "github.com/akashc777/OneCamp/business/QuerySpec"
)

// Family is how a field's values compare: Field Types' (#60) families, and
// three that only search has (design/QUERY_STACK_CONTRACT.md §2 item 13).
type Family string

// The families.
const (
	FamilyText       Family = "text"
	FamilyNumber     Family = "number"
	FamilyDate       Family = "date"
	FamilyCheckbox   Family = "checkbox"
	FamilyOption     Family = "option"
	FamilyList       Family = "list"
	FamilyPeople     Family = "people"
	FamilyRows       Family = "rows"
	FamilyItems      Family = "items"
	FamilyAttachment Family = "attachment"
	FamilyPlace      Family = "place"
	FamilyFlag       Family = "flag"
	FamilyKind       Family = "kind"
)

// Asker is who a query is read for: the Access Resolver's subject, through
// which every name resolves, and their address, by which the public demo
// keeps people from its shared visitor (helpers.DemoListsPerson).
type Asker struct {
	Subject accessBusiness.Subject
	Email   string
}

// Resolver turns a typed name into what it names, as the asker: none when
// nothing they may open is called that, never an error for that. An error is
// a lookup that couldn't run.
type Resolver func(ctx context.Context, who Asker, scope querySpec.Scope, typed string) ([]querySpec.Value, error)

// Labeler is how a resolved value reads to the asker, or false when they may
// not see what it names.
type Labeler func(ctx context.Context, who Asker, scope querySpec.Scope, v querySpec.Value) (string, bool)

// Field is one field a query may name.
type Field struct {
	// ID is what the tree names it by: the built-in name, or field:<id> for a
	// custom field. Empty is the name.
	ID string
	// Name is how it's typed: lower case for built-ins, as shown for custom
	// fields. Aliases are other names for it.
	Name    string
	Aliases []string
	Family  Family
	// Many is a field that holds several values (labels): its repeated
	// modifiers mean all of them. Otherwise any of them (§4.3).
	Many bool
	// Ordered is an option field whose options compare by their place in its
	// list (priority).
	Ordered bool
	// Group is a status field's group (to_do, in_progress, done): its values
	// compare by status_group_is.
	Group bool
	// Ahead is a date field that looks forward (due, start): a weekday or a
	// month name means the next one.
	Ahead bool
	// Sortable is whether ORDER BY may name it.
	Sortable bool
	// Choices are its fixed values, each with its synonyms. With none, a
	// value is resolved by Resolve, or kept as typed when Resolve is nil.
	Choices []Choice
	// Resolve turns a typed name into what it names (people, places, options).
	Resolve Resolver
	// Label is how a resolved value (one with an ID) reads to the asker, for
	// Format: false when they may not see what it names. A saved view shared
	// with others is written back for each reader through it, so nobody sees
	// the names its owner saw.
	Label Labeler
	// Target and Fixed make a field a modifier for another one with a fixed
	// operator: before: is created with before.
	Target string
	Fixed  string
	// Help is one line for autocomplete.
	Help string
}

func (f Field) id() string {
	if f.ID != "" {
		return f.ID
	}
	return f.Name
}

// Choice is one fixed value of a field.
type Choice struct {
	// Value is what the tree holds; Synonyms are other ways to type it.
	Value    string
	Synonyms []string
	Label    string
}

// ScopeFields gives one scope's custom fields (a project's, a table's) that
// the asker may see, by name: each Field's ID is field:<id>.
type ScopeFields interface {
	Fields(ctx context.Context, who Asker, scope querySpec.Scope) ([]Field, error)
}

type registry struct {
	mu     sync.RWMutex
	byName map[string]*Field // lower-case names and aliases
	order  []*Field
	scopes map[string]ScopeFields
}

// Fields is where every field registers.
var Fields = &registry{byName: map[string]*Field{}, scopes: map[string]ScopeFields{}}

// Register adds a built-in field, once, from its area's init.
func (r *registry) Register(f Field) {
	if !isIdent(f.Name) || f.Family == "" {
		panic(fmt.Sprintf("query: the field %q needs a name a query can type and a family", f.Name))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored := f
	for _, name := range append([]string{f.Name}, f.Aliases...) {
		key := strings.ToLower(name)
		if !isIdent(key) {
			panic(fmt.Sprintf("query: %q can't be typed as a field name", name))
		}
		if _, taken := r.byName[key]; taken {
			panic(fmt.Sprintf("query: the field name %q is registered twice", name))
		}
		r.byName[key] = &stored
	}
	r.order = append(r.order, &stored)
}

// AddChoices gives a registered field more fixed values: each area's has:,
// is: and type: values. A value or synonym the field has already panics.
func (r *registry) AddChoices(field string, choices ...Choice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.byName[strings.ToLower(field)]
	if !ok {
		panic(fmt.Sprintf("query: no field %q to add values to", field))
	}
	for _, c := range choices {
		for _, word := range append([]string{c.Value}, c.Synonyms...) {
			if _, taken := choiceOf(f.Choices, word); taken {
				panic(fmt.Sprintf("query: %s:%s is registered twice", field, word))
			}
		}
		f.Choices = append(f.Choices, c)
	}
}

// RegisterScope sets the provider of a kind of scope's custom fields, once.
func (r *registry) RegisterScope(kind string, s ScopeFields) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, taken := r.scopes[kind]; taken {
		panic(fmt.Sprintf("query: the scope %q has two field providers", kind))
	}
	r.scopes[kind] = s
}

func (r *registry) lookup(name string) (Field, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	f, ok := r.byName[strings.ToLower(name)]
	if !ok {
		return Field{}, false
	}
	return *f, true
}

func (r *registry) scopeFields(kind string) (ScopeFields, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.scopes[kind]
	return s, ok
}

// names is every field name and alias, sorted, for suggestions.
func (r *registry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byName))
	for name := range r.byName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// choiceOf is the choice typed matches, by value or synonym, without case.
func choiceOf(choices []Choice, typed string) (Choice, bool) {
	for _, c := range choices {
		if strings.EqualFold(c.Value, typed) {
			return c, true
		}
		for _, s := range c.Synonyms {
			if strings.EqualFold(s, typed) {
				return c, true
			}
		}
	}
	return Choice{}, false
}
