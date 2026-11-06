package business

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// Behaviour is what an operation does to the world, in MCP's vocabulary
// (readOnlyHint, destructiveHint, idempotentHint). Declared once, it decides
// REST's write handling (Idempotency-Key, the write limit: an op is a write
// exactly when it isn't ReadOnly) and MCP's hints, so the two never disagree.
type Behaviour struct {
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	// Recoverable is a write the API itself takes back, through the ops the
	// op's UndoneBy names, published wherever it is: a delete's rows come back
	// from the trash, an overwrite is set back from the row's history. MCP's
	// ladder asks no person to approve a Destructive op that is Recoverable
	// (business/MCPServer RequiresApproval), as it asks none for an additive
	// write; destructiveHint still says Destructive. An op whose undo isn't
	// published beside it is never Recoverable (CheckUndo). A Recoverable
	// call's audit row names the change it makes (invoke).
	Recoverable bool
}

// Op is one operation of the public API (tables H1; API_CONTRACT §3.2),
// declared once and published twice: as a /v1 route in the new shape
// (RegisterOps) and, in the AI edition, as a governed MCP tool with the same
// scope (business/MCPServer PublishOps).
type Op struct {
	// Name is stable ("tables.rows.query"); MCP's tool name is derived from it.
	Name string
	// Summary is one line of STATIC text: it reaches a model as a tool's
	// description.
	Summary      string
	Method, Path string
	Scope        string
	Behaviour    Behaviour
	// Input is the arguments' JSON Schema, STATIC text, enforced alike on REST
	// and MCP; a keyword the validator doesn't enforce is refused when the op
	// registers, so a declared rule always holds.
	Input json.RawMessage
	// Exactly one of Reach, AppsInside or AppsRefused, as on a Route. Reach
	// reads the arguments Serve acts on, never the request.
	Reach       func(args map[string]any) (kind, id string)
	AppsInside  bool
	AppsRefused string
	// Unplaced is what an op that acts on no existing place acts on instead,
	// for MCP's authority (business/MCPServer opKinds): "self", a thing it
	// makes that will be the caller's own (a table); "workspace", a read of
	// only what the caller may see. Such an op has no Reach, and an app is
	// refused it (AppsRefused says why): an app has no things of its own.
	Unplaced string
	// Cost is what a call spends of the token's row budget (ChargeRows),
	// worked out once the arguments are valid: at least 1; nil is 1.
	Cost func(args map[string]any) int
	// UndoneBy names the ops that take a Recoverable op's writes back, and
	// only a Recoverable op's: at least one of them a write.
	UndoneBy  []string
	Versioned bool
	Limit     Limit
	Serve     func(ctx context.Context, call Call) (any, error)

	// input is Input compiled, by RegisterOps.
	input *schema
}

// Call is what an op's Serve is handed: its arguments, built once per request
// from the path's parameters and the body (a path parameter wins; a body that
// contradicts one is refused), and for a versioned op, the version If-Match
// named, as the "version" argument.
type Call struct {
	Args map[string]any
}

// ops is every registered op, in registration order (under mu).
var ops []Op

// RegisterOps publishes ops as res's routes in the new shape, at its package's
// init, beside any routes res declares itself. Each op becomes one route: a
// write exactly when it isn't ReadOnly, holding an app the way the op says,
// its arguments built once and checked against its Input before anything
// reads them. An op without a name, a summary, a scope or Serve, one whose
// Input declares a rule the validator doesn't enforce, or a name registered
// twice panics, as Register does for a route.
func RegisterOps(res Resource, list []Op) {
	routes := append([]Route(nil), res.Routes...)
	seen := map[string]bool{}
	mu.Lock()
	for _, o := range ops {
		seen[o.Name] = true
	}
	mu.Unlock()
	for _, op := range list {
		if op.Name == "" || op.Summary == "" || op.Scope == "" || op.Serve == nil {
			panic(fmt.Sprintf("v1api: op %q (%s) needs a Name, a Summary, a Scope and Serve", op.Name, res.Name))
		}
		if seen[op.Name] {
			panic(fmt.Sprintf("v1api: op %s is registered twice", op.Name))
		}
		if op.Unplaced != "" && (op.Reach != nil || op.AppsRefused == "") {
			panic(fmt.Sprintf("v1api: op %s acts on no place (%s): it has no Reach, and an app is refused it", op.Name, op.Unplaced))
		}
		if b := op.Behaviour; b.Recoverable != (len(op.UndoneBy) > 0) || (b.Recoverable && b.ReadOnly) {
			panic(fmt.Sprintf("v1api: op %s: a write is Recoverable exactly when UndoneBy names the ops that take it back", op.Name))
		}
		seen[op.Name] = true
		in, err := compileSchema(op.Input)
		if err != nil {
			panic(fmt.Sprintf("v1api: op %s (%s): its Input %v", op.Name, res.Name, err))
		}
		op.input = in
		p := &op
		rt := Route{Method: op.Method, Path: op.Path, Summary: op.Summary, Scope: op.Scope, Write: !op.Behaviour.ReadOnly,
			Versioned: op.Versioned, Limit: op.Limit, AppsInside: op.AppsInside, AppsRefused: op.AppsRefused, op: p}
		if op.Reach != nil {
			rt.Reach = func(r *http.Request) (string, string) { return p.Reach(argsOf(r)) }
		}
		routes = append(routes, rt)
	}
	res.Routes = routes
	Register(res)
	mu.Lock()
	defer mu.Unlock()
	for _, rt := range routes {
		if rt.op != nil {
			ops = append(ops, *rt.op)
		}
	}
}

// Ops is every registered op, for MCP's half (PublishOps).
func Ops() []Op {
	mu.Lock()
	defer mu.Unlock()
	return append([]Op(nil), ops...)
}

// CheckUndo refuses a list of ops published together (every op is a REST
// route, and MCP's half publishes the list it is handed) in which a
// Recoverable op names an undo the list hasn't got, or names reads alone: its
// writes could then be made where they can't be taken back.
func CheckUndo(list []Op) error {
	byName := make(map[string]Op, len(list))
	for _, op := range list {
		byName[op.Name] = op
	}
	for _, op := range list {
		if !op.Behaviour.Recoverable {
			continue
		}
		writes := 0
		for _, name := range op.UndoneBy {
			undo, ok := byName[name]
			if !ok {
				return fmt.Errorf("v1api: op %s is Recoverable through %s, which isn't published beside it", op.Name, name)
			}
			if !undo.Behaviour.ReadOnly {
				writes++
			}
		}
		if writes == 0 {
			return fmt.Errorf("v1api: op %s is Recoverable through reads alone (%v)", op.Name, op.UndoneBy)
		}
	}
	return nil
}

// Invoke runs the op named as MCP's half calls it (business/MCPServer
// PublishOps), the way its route does: the arguments checked by the one
// validator, then charged, recorded and served (invoke). ctx is the call's: the
// token's member, the token and the agent it is bound to, as the token's check
// put them there.
func Invoke(ctx context.Context, name string, args map[string]any) (any, error) {
	var op *Op
	mu.Lock()
	for i := range ops {
		if ops[i].Name == name {
			o := ops[i]
			op = &o
		}
	}
	mu.Unlock()
	if op == nil {
		return nil, Errorf(http.StatusNotFound, "not_found", "", "there is no operation %q", name)
	}
	if args == nil {
		args = map[string]any{}
	}
	if err := op.input.check(args); err != nil {
		return nil, err
	}
	return invoke(ctx, op, helpers.GetApiTokenID(ctx), args)
}

// A list's page: limit 1..200, 50 when not given (API_CONTRACT §3.2).
const (
	DefaultPageLimit = 50
	MaxPageLimit     = 200
)

// PageArgs is a list's one page shape, read from a call's arguments: cursor,
// and limit, held to 1..MaxPageLimit (DefaultPageLimit when not given).
func PageArgs(args map[string]any) (cursor string, limit int) {
	cursor, _ = args["cursor"].(string)
	limit = DefaultPageLimit
	if f, _, ok := numberOf(args["limit"]); ok {
		limit = int(math.Max(1, math.Min(MaxPageLimit, f)))
	}
	return cursor, limit
}

// The row budget of a token, a minute (condition b: no cap per table or base;
// every call spends what it reads or writes, a row at a time): a batch of a
// thousand rows spends a thousand.
const (
	RowReadsPerMinute  = 120_000
	RowWritesPerMinute = 60_000
)

// ChargeRows spends n of the token's row budget, reads and writes apart, on
// one counter per token that REST and MCP both spend. A call with no token
// spends nothing, and without Redis every call goes through, as the token's
// own limit does: an outage never takes the API down. A spent budget is a
// 429 that says when to try again.
func ChargeRows(ctx context.Context, tokenID string, readOnly bool, n int) error {
	if tokenID == "" {
		return nil
	}
	spec, budget, did := registry.ApiRowWrites, RowWritesPerMinute, "written"
	if readOnly {
		spec, budget, did = registry.ApiRowReads, RowReadsPerMinute, "read"
	}
	res := redisStore.AllowFixedWindowN(ctx, spec, []string{tokenID}, n, budget)
	if res.Allowed {
		return nil
	}
	after := res.RetryAfterSeconds()
	if after <= 0 {
		after = int(spec.TTL.Seconds())
	}
	return &Error{Status: http.StatusTooManyRequests, Code: "row_budget", RetryAfter: after,
		Hint:    "wait for Retry-After seconds, then send it again",
		Message: fmt.Sprintf("this token has %s %d rows in the last minute, its budget", did, budget)}
}

// chargeRows is ChargeRows, swapped by tests.
var chargeRows = ChargeRows

// audit writes a call's record to the audit log, swapped by tests.
var audit = auditBusiness.RecordForPrincipal

// recordWrite puts a write's call in the audit log before it is served
// (condition e): who made it, meaning the token's member and the agent the
// token is bound to, the token, the op and the place it names, all from the
// context the token's check filled and the arguments already checked. A
// write that can't be recorded isn't made ("no record, no action", as MCP's
// governed calls), and reads aren't recorded: many, and they change nothing.
func recordWrite(ctx context.Context, op *Op, args map[string]any) error {
	info, _ := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	var actor *uuid.UUID
	if id := info.UserPostgresInfo.Id; id != uuid.Nil {
		actor = &id
	}
	kind := auditBusiness.ActorHuman
	meta := map[string]any{"op": op.Name, "token_id": helpers.GetApiTokenID(ctx)}
	if agent := helpers.GetApiAgentID(ctx); agent != "" {
		kind, meta["agent_id"] = auditBusiness.ActorAgent, agent
	}
	if op.Reach != nil {
		meta["resource_kind"], meta["resource_id"] = op.Reach(args)
	}
	if change, ok := helpers.CallChange(ctx); ok {
		meta["change_id"] = change.String()
	}
	if err := audit(ctx, actor, info.UserPostgresInfo.EmailID, kind, "api."+op.Name, auditBusiness.CategoryIntegration,
		"Public API call: "+op.Name, meta); err != nil {
		helpers.LogErrorWithContext(ctx, "business/PublicAPI record %s: %v", op.Name, err)
		return Errorf(http.StatusServiceUnavailable, "unavailable", "send it again shortly",
			"the call couldn't be recorded, so it wasn't made")
	}
	return nil
}

// ErrVersion is a versioned write that names a version the value has moved on
// from.
var ErrVersion = Errorf(http.StatusPreconditionFailed, "version_mismatch", "read it again and send its new version",
	"it changed since the version you sent")

// argsKey holds a call's arguments, as argued built them.
type argsKey struct{}

// argsOf is the arguments argued built for r.
func argsOf(r *http.Request) map[string]any {
	args, _ := r.Context().Value(argsKey{}).(map[string]any)
	return args
}

// argued builds an op's arguments ONCE, before anything reads them, and
// checks them against its Input: Reach, Cost and Serve are all handed this
// one map.
func argued(rt Route, next http.Handler) http.Handler {
	params := pathParam.FindAllStringSubmatch(rt.Path, -1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args, err := buildArgs(r, rt.op, params)
		if err != nil {
			writeError(w, asError(err))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), argsKey{}, args)))
	})
}

// buildArgs is a request's arguments: a read's query or a write's body (one
// JSON object, numbers kept as written), then the path's parameters, then a
// versioned op's If-Match as "version". What the body says about a parameter
// must agree with it: another value is refused, never overridden. The body
// is put back for Idempotency-Key, which reads it again.
func buildArgs(r *http.Request, op *Op, params [][]string) (map[string]any, error) {
	args := map[string]any{}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		for k, vs := range r.URL.Query() {
			args[k] = queryValue(op.input.properties[k], vs)
		}
	} else if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, Errorf(http.StatusBadRequest, "unreadable_body", "", "the request's body couldn't be read: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if len(bytes.TrimSpace(body)) > 0 {
			dec := json.NewDecoder(bytes.NewReader(body))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil || dec.More() {
				return nil, invalid("the body isn't one JSON value")
			}
			obj, ok := v.(map[string]any)
			if !ok {
				return nil, invalid("the body is a JSON object of the call's arguments")
			}
			args = obj
		}
	}
	for _, p := range params {
		if err := put(args, p[1], r.PathValue(p[1]), "the path"); err != nil {
			return nil, err
		}
	}
	if op.Versioned {
		if h := r.Header.Get("If-Match"); h != "" {
			v, ok := versionOf(h)
			if !ok {
				return nil, invalid(`If-Match is the ETag the value was read with: W/"<version>"`)
			}
			if err := put(args, "version", v, "If-Match"); err != nil {
				return nil, err
			}
		}
	}
	if err := op.input.check(args); err != nil {
		return nil, err
	}
	return args, nil
}

// put sets args[name] from the path or If-Match: a body that already says the
// same is the same argument; one that says otherwise is refused.
func put(args map[string]any, name string, v any, from string) error {
	if had, ok := args[name]; ok && fmt.Sprint(had) != fmt.Sprint(v) {
		return Errorf(http.StatusBadRequest, "conflicting_argument", "leave it out of the body, or send the same value",
			"the body's %s isn't the one %s names", name, from)
	}
	args[name] = v
	return nil
}

// versionOf reads If-Match's version: W/"7" (or "7") is 7.
func versionOf(h string) (json.Number, bool) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "W/")
	if len(h) < 3 || h[0] != '"' || h[len(h)-1] != '"' {
		return "", false
	}
	n := h[1 : len(h)-1]
	if _, err := strconv.ParseUint(n, 10, 63); err != nil {
		return "", false
	}
	return json.Number(n), true
}

// queryValue is a query parameter as the argument its schema declares: a
// number stays as written for the check, a list keeps every value, and two
// values for anything else are left as a list, which the check refuses.
func queryValue(s *schema, vs []string) any {
	list := make([]any, len(vs))
	for i, v := range vs {
		list[i] = v
	}
	if s != nil && s.typ == "array" {
		return list
	}
	if len(vs) != 1 {
		return list
	}
	if s != nil {
		switch s.typ {
		case "integer", "number":
			return json.Number(vs[0])
		case "boolean":
			if b, err := strconv.ParseBool(vs[0]); err == nil {
				return b
			}
		}
	}
	return vs[0]
}

// invoke runs a call whose arguments are valid: its cost (at least 1) is
// charged to the token's row budget, a write is recorded, then it is served.
func invoke(ctx context.Context, op *Op, tokenID string, args map[string]any) (any, error) {
	n := 1
	if op.Cost != nil {
		n = max(1, op.Cost(args))
	}
	if err := chargeRows(ctx, tokenID, op.Behaviour.ReadOnly, n); err != nil {
		return nil, err
	}
	if !op.Behaviour.ReadOnly {
		if op.Behaviour.Recoverable {
			// Named before it is recorded: the audit row and the change set
			// the write makes carry the same id.
			ctx = helpers.WithCallChange(ctx, uuid.New())
		}
		if err := recordWrite(ctx, op, args); err != nil {
			return nil, err
		}
	}
	return op.Serve(ctx, Call{Args: args})
}

// serveOp answers an op's route: invoke on the arguments argued built.
func serveOp(rt Route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, err := invoke(r.Context(), rt.op, helpers.GetApiTokenID(r.Context()), argsOf(r))
		answer(w, r, rt, v, err)
	})
}

func invalid(format string, args ...any) *Error {
	return Errorf(http.StatusBadRequest, "invalid_argument", "the call's arguments are in the reference, GET /v1/openapi.json", format, args...)
}

// schema is an op's Input, compiled: the part of JSON Schema this API
// enforces, alike on REST and MCP. A schema using anything else doesn't
// compile, so no declared rule is ever skipped; an argument it doesn't
// declare is refused.
type schema struct {
	typ        string
	properties map[string]*schema
	// rest is what each key an inner object doesn't declare keeps
	// (additionalProperties as a schema): a map, such as a row's values by field.
	rest                 *schema
	required             []string
	items                *schema
	minItems, maxItems   *int
	minLength, maxLength *int
	minimum, maximum     *float64
	enum                 []any
}

// schemaWords are the keywords the check enforces, and the annotations it
// reads past.
var schemaWords = map[string]bool{"type": true, "properties": true, "required": true, "items": true, "minItems": true,
	"maxItems": true, "minLength": true, "maxLength": true, "minimum": true, "maximum": true, "enum": true,
	"additionalProperties": true, "description": true, "title": true}

// compileSchema compiles an op's Input: an object of named arguments.
func compileSchema(raw json.RawMessage) (*schema, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("is missing: an op declares its arguments")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("isn't JSON: %v", err)
	}
	s, err := compileNode(v, "the arguments")
	if err != nil {
		return nil, err
	}
	if s.typ != "object" {
		return nil, fmt.Errorf("is an object: a call's arguments are named")
	}
	if s.rest != nil {
		return nil, fmt.Errorf("names each argument: its additionalProperties may only be false, so an undeclared argument is always refused")
	}
	return s, nil
}

func compileNode(v any, at string) (*schema, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: a schema is an object", at)
	}
	for _, k := range sortedKeys(m) {
		if !schemaWords[k] {
			return nil, fmt.Errorf("%s: the keyword %q isn't enforced here, so it can't be declared", at, k)
		}
	}
	s := &schema{}
	t, typed := m["type"]
	s.typ, _ = t.(string)
	fits := map[string]string{"properties": "object", "required": "object", "additionalProperties": "object", "items": "array",
		"minItems": "array", "maxItems": "array", "minLength": "string", "maxLength": "string"}
	switch s.typ {
	case "object", "array", "string", "integer", "number", "boolean":
	default:
		if typed {
			return nil, fmt.Errorf("%s: type is one of object, array, string, integer, number or boolean", at)
		}
		// No type: any JSON value, null included, which the op checks itself
		// (a cell's value, by its field's kind). It declares nothing a type would.
	}
	for k, typ := range fits {
		if _, ok := m[k]; ok && s.typ != typ {
			return nil, fmt.Errorf("%s: %s is for an %s", at, k, typ)
		}
	}
	if _, ok := m["minimum"]; ok && s.typ != "integer" && s.typ != "number" {
		return nil, fmt.Errorf("%s: minimum is for a number", at)
	}
	if _, ok := m["maximum"]; ok && s.typ != "integer" && s.typ != "number" {
		return nil, fmt.Errorf("%s: maximum is for a number", at)
	}
	if ap, ok := m["additionalProperties"]; ok && ap != false {
		if _, isSchema := ap.(map[string]any); !isSchema {
			return nil, fmt.Errorf("%s: additionalProperties is false, or the schema each other key's value keeps", at)
		}
		c, err := compileNode(ap, at+"{}")
		if err != nil {
			return nil, err
		}
		s.rest = c
	}
	if p, ok := m["properties"]; ok {
		pm, ok := p.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: properties is an object", at)
		}
		s.properties = map[string]*schema{}
		for _, name := range sortedKeys(pm) {
			c, err := compileNode(pm[name], at+"."+name)
			if err != nil {
				return nil, err
			}
			s.properties[name] = c
		}
	}
	if req, ok := m["required"]; ok {
		list, _ := req.([]any)
		for _, x := range list {
			name, _ := x.(string)
			if s.properties[name] == nil {
				return nil, fmt.Errorf("%s: required names %v, which it doesn't declare", at, x)
			}
			s.required = append(s.required, name)
		}
	}
	if it, ok := m["items"]; ok {
		c, err := compileNode(it, at+"[]")
		if err != nil {
			return nil, err
		}
		s.items = c
	}
	if s.typ == "array" && s.items == nil {
		return nil, fmt.Errorf("%s: an array declares its items", at)
	}
	for k, dst := range map[string]**int{"minItems": &s.minItems, "maxItems": &s.maxItems, "minLength": &s.minLength, "maxLength": &s.maxLength} {
		if x, ok := m[k]; ok {
			n, err := strconv.Atoi(fmt.Sprint(x))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s: %s is a whole number, 0 or more", at, k)
			}
			*dst = &n
		}
	}
	for k, dst := range map[string]**float64{"minimum": &s.minimum, "maximum": &s.maximum} {
		if x, ok := m[k]; ok {
			f, _, ok := numberOf(x)
			if !ok {
				return nil, fmt.Errorf("%s: %s is a number", at, k)
			}
			*dst = &f
		}
	}
	if e, ok := m["enum"]; ok {
		list, _ := e.([]any)
		if len(list) == 0 {
			return nil, fmt.Errorf("%s: enum lists its values", at)
		}
		s.enum = list
	}
	return s, nil
}

// check is whether args keep the schema: a 400 invalid_argument that names
// the first argument that doesn't.
func (s *schema) check(args map[string]any) error {
	if why := s.keep(args, ""); why != "" {
		return invalid("%s", why)
	}
	return nil
}

// keep is why v doesn't keep s, at the argument named at; "" when it does.
func (s *schema) keep(v any, at string) string {
	name := at
	if name == "" {
		name = "the arguments"
	}
	switch s.typ {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return name + " is an object"
		}
		for _, k := range sortedKeys(m) {
			c := s.properties[k]
			if c == nil {
				c = s.rest
			}
			if c == nil {
				return fmt.Sprintf("%s isn't an argument of this call", field(at, k))
			}
			if why := c.keep(m[k], field(at, k)); why != "" {
				return why
			}
		}
		for _, k := range s.required {
			if _, ok := m[k]; !ok {
				return field(at, k) + " is required"
			}
		}
	case "array":
		list, ok := v.([]any)
		if !ok {
			return name + " is a list"
		}
		if s.maxItems != nil && len(list) > *s.maxItems {
			return fmt.Sprintf("%s has at most %d items", name, *s.maxItems)
		}
		if s.minItems != nil && len(list) < *s.minItems {
			return fmt.Sprintf("%s has at least %d items", name, *s.minItems)
		}
		for i, x := range list {
			if why := s.items.keep(x, fmt.Sprintf("%s[%d]", name, i)); why != "" {
				return why
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return name + " is a string"
		}
		n := utf8.RuneCountInString(str)
		if s.maxLength != nil && n > *s.maxLength {
			return fmt.Sprintf("%s is at most %d characters", name, *s.maxLength)
		}
		if s.minLength != nil && n < *s.minLength {
			return fmt.Sprintf("%s is at least %d characters", name, *s.minLength)
		}
	case "integer", "number":
		f, whole, ok := numberOf(v)
		if !ok {
			return name + " is a number"
		}
		if s.typ == "integer" && !whole {
			return name + " is a whole number"
		}
		if s.minimum != nil && f < *s.minimum {
			return fmt.Sprintf("%s is at least %v", name, *s.minimum)
		}
		if s.maximum != nil && f > *s.maximum {
			return fmt.Sprintf("%s is at most %v", name, *s.maximum)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return name + " is true or false"
		}
	}
	if s.enum != nil {
		for _, e := range s.enum {
			if fmt.Sprint(e) == fmt.Sprint(v) {
				return ""
			}
		}
		return fmt.Sprintf("%s is one of %v", name, s.enum)
	}
	return ""
}

func field(at, k string) string {
	if at == "" {
		return k
	}
	return at + "." + k
}

// numberOf reads a number as JSON decodes it (json.Number with UseNumber,
// float64 without; an int from Go): its value, and whether it is whole.
func numberOf(v any) (f float64, whole, ok bool) {
	switch n := v.(type) {
	case json.Number:
		x, err := n.Float64()
		if err != nil {
			return 0, false, false
		}
		f = x
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, false, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false, false
	}
	return f, f == math.Trunc(f), true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
