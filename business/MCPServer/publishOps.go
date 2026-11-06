package business

// Publishing the public API's operations as governed tools (tables H1, part C;
// the AI edition, as this whole package is).
//
// An operation (business/PublicAPI Op) is declared once. RegisterOps makes it a
// /v1 route; PublishOps makes it a tool here, under the same governed ladder as
// every bridged tool: the op's scope, its behaviour (so the same hints), its
// Input schema and summary, a resource from its Reach, and a handler that runs
// it through v1api.Invoke, the path its route serves it by: the one validator,
// the token's row budget, the audit record, then Serve as the token's member.
// So a call sees and changes exactly what it would over REST (condition c).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
)

// opKinds is the authority each Reach kind of a published op is checked under.
// Closed: an op whose Reach names a kind missing here fails startup, as a
// bridged tool without a rule does. A table's ops act on its rows, and Serve
// holds any op that changes the table itself to CanManage, as on REST.
//
// An op that acts on no existing place (v1api.Op Unplaced) is checked as a
// bridged tool of that kind is: "self" makes something the caller will own
// (a table), "workspace" reads only what the caller may see, and neither
// names an object.
var opKinds = map[string]ResourceKind{"table": ResourceTableRows, "self": ResourceSelfOwned, "workspace": ResourceWorkspace}

// published are the tools PublishOps registered, in the API's order: tools/list
// lists them beside ai.ToolRegistry's (controllers/MCP listToolsForScopes).
var published []*ToolSpec

// PublishOps registers every op of the public API as a governed tool, once at
// startup and after RegisterBridgedTools. An op that can't be published (no
// Reach, a kind with no rule, a name another tool has) fails startup rather
// than leave a catalogue that depends on how far startup got.
func PublishOps() error {
	return publish(v1api.Ops())
}

func publish(ops []v1api.Op) error {
	// A Recoverable op runs with no person's approval because its undo is
	// published beside it: here, the same list.
	if err := v1api.CheckUndo(ops); err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	specs := make([]*ToolSpec, 0, len(ops))
	for _, op := range ops {
		spec, err := specForOp(op)
		if err != nil {
			return err
		}
		if _, taken := staticToolDef(spec.Name); taken {
			return fmt.Errorf("mcp: op %s publishes as %q, a tool ai.ToolRegistry already names", op.Name, spec.Name)
		}
		if err := Register(spec); err != nil {
			return fmt.Errorf("mcp: op %s: %w", op.Name, err)
		}
		specs = append(specs, spec)
	}
	registryMu.Lock()
	published = append(published, specs...)
	registryMu.Unlock()
	return nil
}

// PublishedTools are the ops PublishOps made tools, for tools/list.
func PublishedTools() []*ToolSpec {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return append([]*ToolSpec(nil), published...)
}

// ToolNameForOp is an op's tool name: tables.rows.query is tables_rows_query.
func ToolNameForOp(op string) string { return strings.ReplaceAll(op, ".", "_") }

// specForOp is one op as a governed tool: everything from its declaration.
func specForOp(op v1api.Op) (*ToolSpec, error) {
	reachKind := op.Unplaced
	switch {
	case op.Reach != nil && reachKind != "":
		return nil, fmt.Errorf("mcp: op %s names a place (Reach) and none (Unplaced)", op.Name)
	case op.Reach != nil:
		reachKind, _ = op.Reach(map[string]any{})
	case reachKind == "":
		return nil, fmt.Errorf("mcp: op %s names no place it acts on (Reach), so nothing can be authorised", op.Name)
	}
	kind, ok := opKinds[reachKind]
	if !ok {
		return nil, fmt.Errorf("mcp: op %s acts on a %q, which has no authority rule here", op.Name, reachKind)
	}
	b := ToolBehaviour{ReadOnly: op.Behaviour.ReadOnly, Destructive: op.Behaviour.Destructive, Idempotent: op.Behaviour.Idempotent,
		Recoverable: op.Behaviour.Recoverable}
	params, err := inputParams(op)
	if err != nil {
		return nil, err
	}
	return &ToolSpec{
		Name:          ToolNameForOp(op.Name),
		Description:   op.Summary,
		InputSchema:   op.Input,
		RequiredScope: op.Scope,
		Behaviour:     b,
		Resource: func(args map[string]any) (ResourceRef, error) {
			if kind.IdentifiesNoObject() {
				return ResourceRef{Kind: kind}, nil
			}
			_, id := op.Reach(args)
			if strings.TrimSpace(id) == "" {
				// A refusal, never a wider scope: see resourceResolver.
				return ResourceRef{}, fmt.Errorf("the %s's id is required", reachKind)
			}
			return ResourceRef{Kind: kind, ID: id}, nil
		},
		// The bridged tools' rule, over the op's declared arguments.
		IdempotencyKey: idempotencyKeyFor(ai.ToolDef{Name: op.Name, Parameters: params}, b),
		Handler:        opHandler(op.Name).serve,
	}, nil
}

// opHandler is a published op as its tool's handler. specForOp takes serve
// as a method value, once at startup, and the governed path calls it per
// tool call, so publish's loop holds a reference and makes no call that
// reaches a store.
type opHandler string

func (op opHandler) serve(ctx context.Context, in ToolCallContext) (any, error) {
	return serveOp(ctx, string(op), in)
}

// inputParams are the arguments an op's Input declares, sorted, as the
// idempotency key reads them.
func inputParams(op v1api.Op) ([]ai.ToolParam, error) {
	var in struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(op.Input, &in); err != nil {
		return nil, fmt.Errorf("mcp: op %s: its Input isn't a schema: %w", op.Name, err)
	}
	names := make([]string, 0, len(in.Properties))
	for k := range in.Properties {
		names = append(names, k)
	}
	sort.Strings(names)
	params := make([]ai.ToolParam, len(names))
	for i, n := range names {
		params[i] = ai.ToolParam{Name: n}
	}
	return params, nil
}

// serveOp runs a published op for the principal the ladder authorised. Its
// context is the /v1/mcp request's, where the API token's check put the
// token's member, the token and its agent: one that names anyone else is
// refused rather than run as them.
func serveOp(ctx context.Context, op string, in ToolCallContext) (any, error) {
	info, _ := ctx.Value(helpers.UserInfoContextKey).(userModels.UserInfo)
	if info.UserPostgresInfo.Id.String() != in.PrincipalUserID || helpers.GetApiTokenID(ctx) != in.TokenID {
		return nil, fmt.Errorf("mcp: %s: the call's context is not the authorised principal's", op)
	}
	out, err := v1api.Invoke(ctx, op, in.Args)
	var refused *v1api.Error
	if errors.As(err, &refused) {
		return nil, &ToolRefusal{Code: refused.Code, Message: refused.Message, Hint: refused.Hint, Details: refused.Details,
			RetryAfter: refused.RetryAfter}
	}
	return out, err
}

// ToolRefusal is a refusal a handler wrote for its caller, in the public API's
// words (a filter that isn't one, a version moved on, a spent budget): the
// governed path forwards it as the tool's error, where any other handler error
// is only "the tool could not complete".
type ToolRefusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// Details are the refusal's parts the caller fixes one by one (the rows a
	// batch refused), as the API's error body carries them.
	Details    any `json:"details,omitempty"`
	RetryAfter int `json:"retry_after,omitempty"`
}

func (r *ToolRefusal) Error() string { return r.Code + ": " + r.Message }

// Text is the refusal as a client that hands its model only text reads it:
// the message, then each of its details on a line of its own, as JSON. Without
// them a model told to fix the rows the details name is shown none.
func (r *ToolRefusal) Text() string {
	if r.Details == nil {
		return r.Message
	}
	raw, err := json.Marshal(r.Details)
	if err != nil {
		return r.Message
	}
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		parts = []json.RawMessage{raw}
	}
	var b strings.Builder
	b.WriteString(r.Message)
	for _, p := range parts {
		b.WriteByte('\n')
		b.Write(p)
	}
	return helpers.TruncateRunes(b.String(), maxToolOutputRunes)
}

// ResultText is a governed tool's result as text, for clients that show only
// text: a map's own "text" (a bridged tool's), or any other value's JSON (a
// published op's page or row), cut at maxToolOutputRunes; structuredContent
// carries the whole value.
func ResultText(out any) string {
	if m, ok := out.(map[string]any); ok {
		s, _ := m["text"].(string)
		return s
	}
	if out == nil {
		return ""
	}
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return helpers.TruncateRunes(string(b), maxToolOutputRunes)
}
