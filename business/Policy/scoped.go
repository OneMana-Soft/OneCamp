package business

// Scoped policies: a policy's value set for one thing beside the workspace
// (a project's own "Who can change tasks"), in policy_scoped_values (migration
// 338). A value set for a scope beats the workspace's there; with none, the
// workspace's applies, then the default. Only a policy whose spec names the
// scope's kind takes one, and setting it is checked, stored and recorded
// exactly as a workspace change is (Set), with the scope beside it.
//
// Who may set a scope's value is the caller's to decide (a project's admins
// for a project's); this file only keeps the values.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	"github.com/google/uuid"
)

// ErrNotScoped is a policy that can't be set for that kind of thing.
var ErrNotScoped = fmt.Errorf("%w: it can't be set for that", ErrBadValue)

// Seams for tests: where scoped values are read and written.
var (
	readScoped = func(ctx context.Context, s Scope) (map[string]string, error) {
		id, err := uuid.Parse(s.ID)
		if err != nil || !connected() {
			return map[string]string{}, nil
		}
		return configModels.ScopedValues(ctx, s.Kind, id)
	}
	writeScoped = func(ctx context.Context, s Scope, key, value string, by uuid.UUID) error {
		id, err := uuid.Parse(s.ID)
		if err != nil {
			return err
		}
		if !connected() {
			return errNoStore
		}
		return configModels.SetScopedValue(ctx, s.Kind, id, key, value, by)
	}
	// readScopedMany is the values set for many scopes of one kind, by
	// lower-case id, in one read (PrimeScopes).
	readScopedMany = func(ctx context.Context, kind string, ids []string) (map[string]map[string]string, error) {
		if !connected() {
			return map[string]map[string]string{}, nil
		}
		var parsed []uuid.UUID
		for _, id := range ids {
			if p, err := uuid.Parse(id); err == nil {
				parsed = append(parsed, p)
			}
		}
		got, err := configModels.ScopedValuesMany(ctx, kind, parsed)
		if err != nil {
			return nil, err
		}
		out := make(map[string]map[string]string, len(got))
		for id, v := range got {
			out[id.String()] = v
		}
		return out, nil
	}
	clearScoped = func(ctx context.Context, s Scope, key string) error {
		id, err := uuid.Parse(s.ID)
		if err != nil {
			return err
		}
		if !connected() {
			return errNoStore
		}
		return configModels.ClearScopedValue(ctx, s.Kind, id, key)
	}
)

// errNoStore is a write with no database to write to.
var errNoStore = errors.New("policy: there's no database to keep it in")

// connected is whether there is a database to read: without one (a test, a
// server still starting) nothing is set anywhere, so the defaults apply, as
// the workspace's values do (registry.go's readValues).
func connected() bool {
	return postgresInit.DBConn != nil && postgresInit.DBConn.SqlDB != nil
}

// scopedCache keeps each scope's values for cacheFor, as the workspace's are
// kept, so a change reaches every instance within that time.
var scopedCache = helpers.NewTTLCache[map[string]string](cacheFor)

func (s Scope) cacheKey() string { return s.Kind + ":" + strings.ToLower(s.ID) }

// scopedValue is key's value set for s, through the cache. A read that fails
// answers none, so the workspace's value applies: what the server did before
// scoped values existed.
func (r *Registry) scopedValue(ctx context.Context, s Scope, key string) (string, bool) {
	if s.Kind == "" || s.ID == "" {
		return "", false
	}
	values, ok := scopedCache.Get(s.cacheKey())
	if !ok {
		got, err := readScoped(ctx, s)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Policy: the values set for %s %s couldn't be read, so the workspace's apply: %+v", s.Kind, s.ID, err)
			return "", false
		}
		values = got
		scopedCache.Set(s.cacheKey(), values)
	}
	v, ok := values[key]
	return v, ok
}

// PrimeScopes reads the values set for every one of ids (scopes of kind)
// not already kept, in one read, and keeps them, those with none included: a
// page of tasks across fifty projects asks once, not fifty times (PR1, the BE
// audit's #2). A failed read keeps nothing; each scope then reads its own.
func (r *Registry) PrimeScopes(ctx context.Context, kind string, ids []string) {
	var want []string
	seen := map[string]bool{}
	for _, id := range ids {
		s := Scope{Kind: kind, ID: id}
		key := s.cacheKey()
		if id == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := scopedCache.Get(key); !ok {
			want = append(want, strings.ToLower(id))
		}
	}
	if len(want) == 0 {
		return
	}
	got, err := readScopedMany(ctx, kind, want)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Policy: the values set for %d %s scopes couldn't be read at once; each reads its own: %+v", len(want), kind, err)
		return
	}
	for _, id := range want {
		v := got[id]
		if v == nil {
			v = map[string]string{}
		}
		scopedCache.Set(Scope{Kind: kind, ID: id}.cacheKey(), v)
	}
}

// ErrScopesUnread is BoolsIn's answer when the values set for some of the
// scopes asked couldn't be read.
var ErrScopesUnread = errors.New("policy: the values set for these couldn't be read")

// BoolsIn answers p for many scopes of one kind at once, by id as asked:
// each one's own value, else the workspace's, else the default, with one
// read of the values not already kept (PrimeScopes). Unlike BoolIn, a read
// that fails answers ErrScopesUnread rather than the workspace's value, so a
// caller guarding someone's own choice (a person who hides when they're
// busy) can keep it rather than show it.
func (r *Registry) BoolsIn(ctx context.Context, p BoolPolicy, kind string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	r.PrimeScopes(ctx, kind, ids)
	def := r.Bool(ctx, p)
	for _, id := range ids {
		values, ok := scopedCache.Get(Scope{Kind: kind, ID: id}.cacheKey())
		if !ok {
			return nil, ErrScopesUnread
		}
		out[id] = def
		if b, err := strconv.ParseBool(values[p.Key]); err == nil {
			out[id] = b
		}
	}
	return out, nil
}

func (r *Registry) BoolIn(ctx context.Context, p BoolPolicy, s Scope) bool {
	if raw, ok := r.scopedValue(ctx, s, p.Key); ok {
		if b, err := strconv.ParseBool(raw); err == nil {
			return b
		}
	}
	return r.Bool(ctx, p)
}

func (r *Registry) IntIn(ctx context.Context, p IntPolicy, s Scope) int {
	if raw, ok := r.scopedValue(ctx, s, p.Key); ok {
		if n, err := strconv.Atoi(raw); err == nil {
			return n
		}
	}
	return r.Int(ctx, p)
}

func (r *Registry) ChoiceIn(ctx context.Context, p ChoicePolicy, s Scope) string {
	if raw, ok := r.scopedValue(ctx, s, p.Key); ok && p.Allows(raw) {
		return raw
	}
	return r.Choice(ctx, p)
}

// scopedSpec is the spec for key that can be set for s's kind of thing.
func scopedSpec(key string, s Scope) (*spec, error) {
	for i := range specs {
		if specs[i].key != key {
			continue
		}
		if err := specs[i].takes(s); err != nil {
			return nil, err
		}
		return &specs[i], nil
	}
	return nil, ErrUnknownPolicy
}

// takes is whether the spec can be set for s: a kind of scope it names, by
// a real id.
func (sp spec) takes(s Scope) error {
	if !slices.Contains(sp.scopes, s.Kind) {
		return ErrNotScoped
	}
	if _, err := uuid.Parse(s.ID); err != nil {
		return ErrNotScoped
	}
	return nil
}

// InfoIn is a policy as it stands for s: its value there, the workspace's as
// the default it falls back to, and whether s has its own.
func (r *Registry) InfoIn(ctx context.Context, key string, s Scope) (Info, error) {
	sp, err := scopedSpec(key, s)
	if err != nil {
		return Info{}, err
	}
	return r.infoIn(ctx, *sp, s), nil
}

func (r *Registry) infoIn(ctx context.Context, sp spec, s Scope) Info {
	workspace := r.info(ctx, sp)
	in := workspace
	in.Default = workspace.Value
	_, in.IsSet = r.scopedValue(ctx, s, sp.key)
	switch sp.kind {
	case KindBool:
		in.Value = r.BoolIn(ctx, BoolPolicy{Key: sp.key, Default: sp.def.(bool)}, s)
	case KindInt:
		in.Value = r.IntIn(ctx, IntPolicy{Key: sp.key, Default: sp.def.(int)}, s)
	case KindChoice:
		in.Value = r.ChoiceIn(ctx, sp.choice(), s)
	case KindObject:
		in.Value = nil
		if v, ok := r.scopedValue(ctx, s, sp.key); ok {
			in.Value = json.RawMessage(v)
		}
	}
	return in
}

// SetIn sets a policy's value for s (JSON, as Set takes it) and records the
// change, with the scope, as Set does; a change the audit log can't record is
// undone and refused.
func (r *Registry) SetIn(ctx context.Context, key string, s Scope, value json.RawMessage, by Changer) (Info, error) {
	sp, err := scopedSpec(key, s)
	if err != nil {
		return Info{}, err
	}
	return r.setScoped(ctx, sp, s, value, by)
}

// SetObjectIn keeps an object policy's value for s, for its own panel only
// (owner): SetIn never finds one, so no general route writes around the
// panel's checks. Checked, recorded and undone as SetIn is.
func (r *Registry) SetObjectIn(ctx context.Context, owner string, p ObjectPolicy, s Scope, value json.RawMessage, by Changer) error {
	sp, err := ownedSpec(owner, p.Key)
	if err != nil {
		return err
	}
	if err := sp.takes(s); err != nil {
		return err
	}
	_, err = r.setScoped(ctx, sp, s, value, by)
	return err
}

// ObjectIn reads an object policy's value for s into into; set is false when
// s keeps none. It comes from s's cached values, the one read every policy of
// the scope shares (a project's triage switch fills it on arrival).
func (r *Registry) ObjectIn(ctx context.Context, p ObjectPolicy, s Scope, into any) (set bool, err error) {
	v, ok := r.scopedValue(ctx, s, p.Key)
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal([]byte(v), into); err != nil {
		return false, fmt.Errorf("business/Policy: %s for %s %s: %w", p.Key, s.Kind, s.ID, err)
	}
	return true, nil
}

// setScoped is SetIn for a spec already found.
func (r *Registry) setScoped(ctx context.Context, sp *spec, s Scope, value json.RawMessage, by Changer) (Info, error) {
	key := sp.key
	stored, shown, err := canonical(*sp, value)
	if err != nil {
		return Info{}, err
	}
	before := r.infoIn(ctx, *sp, s)
	prev, wasSet := r.scopedValue(ctx, s, key)
	if err := writeScoped(ctx, s, key, stored, by.UserID); err != nil {
		return Info{}, err
	}
	scopedCache.Delete(s.cacheKey())
	if err := r.recordScoped(ctx, *sp, s, before.Value, storedJSON(*sp, stored), shown, by); err != nil {
		var undo error
		if wasSet {
			undo = writeScoped(ctx, s, key, prev, by.UserID)
		} else {
			undo = clearScoped(ctx, s, key)
		}
		if undo != nil {
			helpers.LogErrorWithContext(ctx, "business/Policy: undoing the change to %s for %s %s failed too: %+v", key, s.Kind, s.ID, undo)
		}
		scopedCache.Delete(s.cacheKey())
		return Info{}, ErrNotRecorded
	}
	afterSetIn(ctx, key, s, storedJSON(*sp, stored), by)
	return r.infoIn(ctx, *sp, s), nil
}

// SetInHook is what follows a recorded SetIn of a key (OnSetIn): the scope,
// the value it was set to (JSON) and who set it. Its error is logged; the
// change stands, as it was recorded.
type SetInHook func(ctx context.Context, s Scope, value json.RawMessage, by Changer) error

var (
	setInMu    sync.RWMutex
	setInHooks = map[string][]SetInHook{}
)

// OnSetIn calls fn after every recorded SetIn of the key, from an init of the
// package the key's setting matters to (business/SLA keeps a project's zone
// from the admin who turns its timers on).
func OnSetIn(key string, fn SetInHook) {
	setInMu.Lock()
	defer setInMu.Unlock()
	setInHooks[key] = append(setInHooks[key], fn)
}

func afterSetIn(ctx context.Context, key string, s Scope, value json.RawMessage, by Changer) {
	setInMu.RLock()
	hooks := setInHooks[key]
	setInMu.RUnlock()
	for _, fn := range hooks {
		if err := fn(ctx, s, value, by); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Policy: what follows setting %s for %s %s failed: %+v", key, s.Kind, s.ID, err)
		}
	}
}

// ClearIn takes s's own value away, so the workspace's applies there again,
// and records it; a change the audit log can't record is undone and refused.
func (r *Registry) ClearIn(ctx context.Context, key string, s Scope, by Changer) (Info, error) {
	sp, err := scopedSpec(key, s)
	if err != nil {
		return Info{}, err
	}
	prev, wasSet := r.scopedValue(ctx, s, key)
	if !wasSet {
		return r.infoIn(ctx, *sp, s), nil
	}
	before := r.infoIn(ctx, *sp, s)
	if err := clearScoped(ctx, s, key); err != nil {
		return Info{}, err
	}
	scopedCache.Delete(s.cacheKey())
	after := r.info(ctx, *sp)
	if err := r.recordScoped(ctx, *sp, s, before.Value, nil, "the workspace's ("+describe(after.Value)+")", by); err != nil {
		if undo := writeScoped(ctx, s, key, prev, by.UserID); undo != nil {
			helpers.LogErrorWithContext(ctx, "business/Policy: undoing the clearing of %s for %s %s failed too: %+v", key, s.Kind, s.ID, undo)
		}
		scopedCache.Delete(s.cacheKey())
		return Info{}, ErrNotRecorded
	}
	return r.infoIn(ctx, *sp, s), nil
}

// recordScoped records a change to a scope's value in the audit log, as Set
// records a workspace's, with the scope in its detail.
func (r *Registry) recordScoped(ctx context.Context, sp spec, s Scope, from any, to json.RawMessage, shown string, by Changer) error {
	summary := fmt.Sprintf("Changed “%s” for a %s from %s to %s", sp.label, s.Kind, describe(from), shown)
	meta := map[string]interface{}{"policy": sp.key, "scope_kind": s.Kind, "scope_id": s.ID, "from": from}
	if to != nil {
		meta["to"] = to
	}
	return recordIt(ctx, &by.UserID, by.Email, auditBusiness.ActorHuman, "policy.changed", auditBusiness.CategorySettings, summary, meta)
}
