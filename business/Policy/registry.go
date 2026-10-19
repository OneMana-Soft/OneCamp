package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/google/uuid"
)

// cacheFor is how long a read of the stored values is trusted.
const cacheFor = 15 * time.Second

// The kinds of policy, as the admin API names them.
const (
	KindBool   = "bool"
	KindInt    = "int"
	KindList   = "list"
	KindChoice = "choice"
)

var (
	// ErrUnknownPolicy is a key no policy has.
	ErrUnknownPolicy = errors.New("policy: there's no such policy")
	// ErrBadValue is a value a policy can't take; its message says why.
	ErrBadValue = errors.New("policy: that value doesn't fit")
	// ErrNotRecorded is a change the audit log couldn't record: it was undone.
	ErrNotRecorded = errors.New("policy: the change couldn't be recorded, so it wasn't made")
)

// spec is one policy as the registry knows it.
type spec struct {
	key, kind, label, help string
	def                    any
	min, max               int      // for an int
	choices                []string // for a choice
	// scopes are the kinds of thing it can be set for beside the workspace
	// (ScopeProject); none, the workspace only.
	scopes []string
}

// specs are every policy, in the order an admin sees them.
var specs = []spec{
	{key: DocsExportMembers.Key, kind: KindBool, def: DocsExportMembers.Default,
		label: "Members can export docs", help: "Admins can always export."},
	{key: DocsPublishMembers.Key, kind: KindBool, def: DocsPublishMembers.Default,
		label: "Members can publish docs to the web", help: "Admins can always publish."},
	{key: GuestLinkMaxDays.Key, kind: KindInt, def: GuestLinkMaxDays.Default, min: 0, max: 3650,
		label: "Longest a guest link lasts, in days", help: "0 sets no limit of its own."},
	{key: EmbedAllowlist.Key, kind: KindList, def: EmbedAllowlist.Default,
		label: "Sites docs can embed", help: "With no list, docs can embed any site they could before."},
	{key: ProjectsEditWho.Key, kind: KindChoice, def: ProjectsEditWho.Default, choices: ProjectsEditWho.Choices, scopes: []string{ScopeProject},
		label: "Who can change tasks", help: "Members, members on the tasks assigned to them, or admins only. Each project can choose its own."},
	{key: ProjectsCreateWho.Key, kind: KindChoice, def: ProjectsCreateWho.Default, choices: ProjectsCreateWho.Choices,
		label: "Who can make projects", help: "A team's admins, or everyone in the team."},
}

// Info is one policy as an admin sees it.
type Info struct {
	Key   string `json:"key"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Help  string `json:"help"`
	// Value is what it is now; Default what it is when nobody set it.
	Value   any  `json:"value"`
	Default any  `json:"default"`
	IsSet   bool `json:"is_set"`
	Min     *int `json:"min,omitempty"`
	Max     *int `json:"max,omitempty"`
	// Choices are a choice's values.
	Choices []string `json:"choices,omitempty"`
}

// Changer is the admin making a change, for its record.
type Changer struct {
	UserID uuid.UUID
	Email  string
}

// Registry is the policies, their stored values and their changes.
type Registry struct {
	mu     sync.Mutex
	values map[string]string // stored values, by key
	readAt time.Time
}

// Admin is the registry, for the admin routes: Policies reads the same one.
var Admin = &Registry{}

// Seams for tests: where values are read and written, and where changes are
// recorded.
var (
	readValues = func(keys []string) (map[string]string, error) {
		if postgresInit.DBConn == nil || postgresInit.DBConn.SqlDB == nil {
			return map[string]string{}, nil
		}
		rows, err := configModels.GetMultipleConfigsByKeys(keys)
		if err != nil {
			return nil, err
		}
		out := map[string]string{}
		for _, r := range rows {
			if r != nil {
				out[r.Key] = r.Value
			}
		}
		return out, nil
	}
	writeValue = configModels.UpsertConfig
	recordIt   = auditBusiness.RecordForPrincipal
	now        = time.Now
)

func (r *Registry) Bool(ctx context.Context, p BoolPolicy) bool {
	raw, ok := r.stored(ctx, p.Key)
	if !ok {
		return p.Default
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return p.Default
	}
	return b
}

func (r *Registry) Int(ctx context.Context, p IntPolicy) int {
	raw, ok := r.stored(ctx, p.Key)
	if !ok {
		return p.Default
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return p.Default
	}
	return n
}

func (r *Registry) List(ctx context.Context, p ListPolicy) []string {
	raw, ok := r.stored(ctx, p.Key)
	if !ok {
		return append([]string(nil), p.Default...)
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return append([]string(nil), p.Default...)
	}
	return out
}

func (r *Registry) Choice(ctx context.Context, p ChoicePolicy) string {
	raw, ok := r.stored(ctx, p.Key)
	if !ok || !p.Allows(raw) {
		return p.Default
	}
	return raw
}

// Require refuses a member while p is off, asked at each request.
func (r *Registry) Require(p BoolPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if isAdmin(req) || r.Bool(req.Context(), p) {
				next.ServeHTTP(w, req)
				return
			}
			helpers.WriteJSON(w, http.StatusForbidden, helpers.Envolope{"msg": "An admin has turned this off for members of this workspace."})
		})
	}
}

// stored is a key's stored value, through the cache.
func (r *Registry) stored(ctx context.Context, key string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.values == nil || now().Sub(r.readAt) > cacheFor {
		keys := make([]string, 0, len(specs))
		for _, s := range specs {
			keys = append(keys, s.key)
		}
		values, err := readValues(keys)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Policy: the policies couldn't be read, so their defaults apply: %+v", err)
			values = map[string]string{}
		}
		r.values, r.readAt = values, now()
	}
	v, ok := r.values[key]
	return v, ok
}

// forget drops the cache, so the next read is fresh.
func (r *Registry) forget() {
	r.mu.Lock()
	r.values = nil
	r.mu.Unlock()
}

// ListAll is every policy, its value and its default.
func (r *Registry) ListAll(ctx context.Context) []Info {
	out := make([]Info, 0, len(specs))
	for _, s := range specs {
		out = append(out, r.info(ctx, s))
	}
	return out
}

func (r *Registry) info(ctx context.Context, s spec) Info {
	in := Info{Key: s.key, Kind: s.kind, Label: s.label, Help: s.help, Default: s.def}
	_, in.IsSet = r.stored(ctx, s.key)
	switch s.kind {
	case KindBool:
		in.Value = r.Bool(ctx, BoolPolicy{Key: s.key, Default: s.def.(bool)})
	case KindInt:
		in.Value = r.Int(ctx, IntPolicy{Key: s.key, Default: s.def.(int)})
		lo, hi := s.min, s.max
		in.Min, in.Max = &lo, &hi
	case KindList:
		def, _ := s.def.([]string)
		in.Value = r.List(ctx, ListPolicy{Key: s.key, Default: def})
	case KindChoice:
		in.Value = r.Choice(ctx, s.choice())
		in.Choices = append([]string(nil), s.choices...)
	}
	return in
}

// choice is a choice spec as the policy it declares. Pure.
func (s spec) choice() ChoicePolicy {
	def, _ := s.def.(string)
	return ChoicePolicy{Key: s.key, Default: def, Choices: s.choices}
}

// Set changes a policy to value (JSON: true, 30, ["youtube.com"], or null for
// a list with no list) and records the change; a change the audit log can't
// record is undone and refused.
func (r *Registry) Set(ctx context.Context, key string, value json.RawMessage, by Changer) (Info, error) {
	var s *spec
	for i := range specs {
		if specs[i].key == key {
			s = &specs[i]
		}
	}
	if s == nil {
		return Info{}, ErrUnknownPolicy
	}
	stored, shown, err := canonical(*s, value)
	if err != nil {
		return Info{}, err
	}
	before := r.info(ctx, *s)
	prev, wasSet := r.stored(ctx, key)
	if err := writeValue(key, stored); err != nil {
		return Info{}, err
	}
	r.forget()
	summary := fmt.Sprintf("Changed “%s” from %s to %s", s.label, describe(before.Value), shown)
	meta := map[string]interface{}{"policy": key, "from": before.Value, "to": storedJSON(*s, stored)}
	if err := recordIt(ctx, &by.UserID, by.Email, auditBusiness.ActorHuman, "policy.changed", auditBusiness.CategorySettings, summary, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Policy: the change to %s couldn't be recorded, so it's undone: %+v", key, err)
		undo := defaultRaw(*s)
		if wasSet {
			undo = prev
		}
		if werr := writeValue(key, undo); werr != nil {
			helpers.LogErrorWithContext(ctx, "business/Policy: undoing the change to %s failed too: %+v", key, werr)
		}
		r.forget()
		return Info{}, ErrNotRecorded
	}
	return r.info(ctx, *s), nil
}

// storedJSON is a stored value as JSON, for a change's record: a choice is
// stored bare, every other kind as JSON already. Pure.
func storedJSON(s spec, stored string) json.RawMessage {
	if s.kind == KindChoice {
		return json.RawMessage(strconv.Quote(stored))
	}
	return json.RawMessage(stored)
}

// hostPattern is a host a list of sites may name: "youtube.com", or a
// wildcard for its subdomains, "*.figma.com".
var hostPattern = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

// maxListItems bounds a list policy.
const maxListItems = 200

// canonical checks value against the policy and answers it as stored and as
// the record shows it. Pure.
func canonical(s spec, value json.RawMessage) (stored, shown string, err error) {
	bad := func(why string) (string, string, error) { return "", "", fmt.Errorf("%w: %s", ErrBadValue, why) }
	switch s.kind {
	case KindBool:
		var b bool
		if json.Unmarshal(value, &b) != nil {
			return bad("it's on or off: true or false")
		}
		return strconv.FormatBool(b), describe(b), nil
	case KindInt:
		var n int
		if json.Unmarshal(value, &n) != nil {
			return bad("it's a whole number")
		}
		if n < s.min || n > s.max {
			return bad(fmt.Sprintf("it's from %d to %d", s.min, s.max))
		}
		return strconv.Itoa(n), describe(n), nil
	case KindList:
		if strings.TrimSpace(string(value)) == "null" {
			return "null", "no list", nil
		}
		var items []string
		if json.Unmarshal(value, &items) != nil {
			return bad("it's a list of sites, like [\"youtube.com\"]")
		}
		if len(items) > maxListItems {
			return bad(fmt.Sprintf("it lists at most %d sites", maxListItems))
		}
		out := []string{}
		seen := map[string]bool{}
		for _, it := range items {
			host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(it)), ".")
			if !hostPattern.MatchString(host) || len(host) > 253 {
				return bad(fmt.Sprintf("%q isn't a site's name, like youtube.com or *.figma.com", it))
			}
			if !seen[host] {
				seen[host] = true
				out = append(out, host)
			}
		}
		raw, _ := json.Marshal(out)
		return string(raw), describe(out), nil
	case KindChoice:
		var v string
		if json.Unmarshal(value, &v) != nil || !s.choice().Allows(v) {
			return bad("it's one of " + strings.Join(s.choices, ", "))
		}
		return v, describe(v), nil
	}
	return bad("unknown kind")
}

// defaultRaw is a policy's default as stored.
func defaultRaw(s spec) string {
	switch v := s.def.(type) {
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case []string:
		if v == nil {
			return "null"
		}
		raw, _ := json.Marshal(v)
		return string(raw)
	case string:
		return v
	}
	return "null"
}

// describe is a value as a change's record reads it. Pure.
func describe(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "on"
		}
		return "off"
	case int:
		return strconv.Itoa(x)
	case []string:
		if x == nil {
			return "no list"
		}
		if len(x) == 0 {
			return "an empty list"
		}
		return strings.Join(x, ", ")
	}
	return fmt.Sprint(v)
}
