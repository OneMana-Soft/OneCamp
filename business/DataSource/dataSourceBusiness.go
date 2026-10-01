// Package business (DataSource) is the management + access layer for external,
// read-only data-source connectors: an admin/agent-manager registers a
// connection to an external SQL database (Postgres to start), and an
// agent/assistant can then query it — read-only — the governed way it already
// queries native Tables.
//
// Permission model (see migration 126), verified against the closest analog
// (MCP servers) and the app's other integration configs:
//
//   - CONFIGURATION (create/update/delete/test/enable + supplying credentials)
//     is capability-gated at the router with agent.manage (admins always,
//     members only when an admin opens it). Within here, manage on a specific
//     source is admin OR its creator (owner).
//   - QUERY access is enforced PER USER via visibility, mirroring Tables:
//     private   -> creator + admins only  (the safe default)
//     workspace -> any member
//     Because the connection authenticates with a SINGLE stored credential, it
//     cannot enforce per-user row security at the external DB, so a source is
//     never shared workspace-wide unless its owner deliberately sets it.
//
// The password is encrypted at rest (helpers.EncryptSecret) and never returned
// to the FE — callers only ever see HasPassword.
package business

import (
	"context"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/DataSource"
	"github.com/google/uuid"
)

const (
	maxNameLen = 120
	maxHostLen = 253
	minPort    = 1
	maxPort    = 65535
)

// Actor is the user performing an action. IsAdmin comes from the auth context;
// the agent-manage capability is enforced at the router for config routes.
type Actor struct {
	UserID  uuid.UUID
	IsAdmin bool
}

var (
	errForbidden = fmt.Errorf("not authorized for this data source")
	errNotFound  = fmt.Errorf("data source not found")
)

// IsForbidden / IsNotFound let controllers map to HTTP codes.
func IsForbidden(err error) bool { return err == errForbidden }
func IsNotFound(err error) bool  { return err == errNotFound }

// canQuery reports whether the actor may QUERY (read/introspect) a source:
// admin, the creator, or any member when the source is workspace-visible.
func canQuery(d *model.DataSource, a Actor) bool {
	return a.IsAdmin || d.CreatedBy == a.UserID || d.Visibility == model.VisibilityWorkspace
}

// canManage reports whether the actor may change a source's config: admin or
// its creator. (The router already gates all config routes with agent.manage.)
func canManage(d *model.DataSource, a Actor) bool {
	return a.IsAdmin || d.CreatedBy == a.UserID
}

// Input is the create/update payload from the controller. Password is a pointer
// so "omitted" (leave unchanged on update) is distinct from "" (clear).
type Input struct {
	Name       string  `json:"name"`
	Engine     string  `json:"engine"`
	Host       string  `json:"host"`
	Port       int     `json:"port"`
	Database   string  `json:"database"`
	Username   string  `json:"username"`
	Password   *string `json:"password"`
	SSLMode    string  `json:"ssl_mode"`
	Visibility string  `json:"visibility"`
	Enabled    *bool   `json:"enabled"`
}

// View is the safe, FE-facing projection of a data source: everything EXCEPT
// the credential, plus HasPassword and CanManage so the UI can render controls.
type View struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	Engine      string `json:"engine"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Database    string `json:"database"`
	Username    string `json:"username"`
	HasPassword bool   `json:"has_password"`
	SSLMode     string `json:"ssl_mode"`
	Visibility  string `json:"visibility"`
	Enabled     bool   `json:"enabled"`
	CreatedBy   string `json:"created_by"`
	CanManage   bool   `json:"can_manage"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

func toView(d *model.DataSource, a Actor) View {
	return View{
		Id:          d.Id.String(),
		Name:        d.Name,
		Engine:      d.Engine,
		Host:        d.Host,
		Port:        d.Port,
		Database:    d.Database,
		Username:    d.Username,
		HasPassword: d.PasswordEnc != nil && strings.TrimSpace(*d.PasswordEnc) != "",
		SSLMode:     d.SSLMode,
		Visibility:  d.Visibility,
		Enabled:     d.Enabled,
		CreatedBy:   d.CreatedBy.String(),
		CanManage:   canManage(d, a),
		CreatedAt:   d.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		UpdatedAt:   d.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
}

// validateConnection normalizes + checks the CONNECTION fields of an input
// (everything needed to actually connect), filling defaults. It is shared by
// create/update validation and the pre-save "test connection" path, so both
// enforce the same engine whitelist, port range, ssl mode, and host allowlist.
func validateConnection(in *Input) error {
	in.Host = strings.TrimSpace(in.Host)
	in.Database = strings.TrimSpace(in.Database)
	in.Username = strings.TrimSpace(in.Username)
	if in.Engine == "" {
		in.Engine = model.EnginePostgres
	}
	if in.SSLMode == "" {
		in.SSLMode = model.SSLRequire
	}
	if !model.ValidEngine(in.Engine) {
		return fmt.Errorf("unsupported engine %q (supported: postgres, mysql)", in.Engine)
	}
	if in.Port == 0 {
		in.Port = model.DefaultPort(in.Engine) // per-engine default (5432 pg / 3306 mysql)
	}
	if in.Host == "" || len(in.Host) > maxHostLen {
		return fmt.Errorf("host is required")
	}
	if in.Port < minPort || in.Port > maxPort {
		return fmt.Errorf("port must be between %d and %d", minPort, maxPort)
	}
	if in.Database == "" {
		return fmt.Errorf("database is required")
	}
	if !model.ValidSSLMode(in.SSLMode) {
		return fmt.Errorf("invalid ssl_mode %q", in.SSLMode)
	}
	// Destination policy (see hostGuard.go): link-local / metadata / multicast /
	// unspecified are always refused, loopback + private ranges only when
	// allowlisted, and any configured allowlist is exhaustive. Enforced here for
	// a fast, actionable message AND again at dial time for every connection.
	if err := checkHostDestination(in.Host); err != nil {
		return err
	}
	return nil
}

// validate is the full create/update validation: connection fields + the
// persisted metadata (name + visibility).
func validate(in *Input) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Visibility == "" {
		in.Visibility = model.VisibilityPrivate
	}
	if in.Name == "" || len(in.Name) > maxNameLen {
		return fmt.Errorf("name is required (max %d chars)", maxNameLen)
	}
	if !model.ValidVisibility(in.Visibility) {
		return fmt.Errorf("invalid visibility %q", in.Visibility)
	}
	return validateConnection(in)
}

// configFromInput builds a decrypted, in-memory Config from a create/update
// input's plaintext fields (used only for the pre-save connection test).
func configFromInput(in Input) Config {
	pw := ""
	if in.Password != nil {
		pw = *in.Password
	}
	return Config{
		Engine:   in.Engine,
		Host:     in.Host,
		Port:     in.Port,
		Database: in.Database,
		Username: in.Username,
		Password: pw,
		SSLMode:  in.SSLMode,
	}
}

// TestConnectionConfig opens a read-only connection from an UNSAVED input and
// pings it, so an operator can validate credentials before saving a source.
// The route is agent.manage gated; the same connection validation (engine, port,
// ssl, host allowlist) as create/update applies, so it opens no host that a
// save wouldn't. Nothing is persisted.
func TestConnectionConfig(ctx context.Context, in Input, _ Actor) error {
	if err := validateConnection(&in); err != nil {
		return err
	}
	conn, nerr := NewConnector(configFromInput(in))
	if nerr != nil {
		return nerr
	}
	defer conn.Close()
	if perr := conn.Ping(ctx); perr != nil {
		return externalErr(opConnect, perr)
	}
	return nil
}

// decryptConfig builds the in-memory (decrypted) connection Config for a source.
// The plaintext password exists only for the lifetime of the connector call.
func decryptConfig(d *model.DataSource) (Config, error) {
	pw := ""
	if d.PasswordEnc != nil {
		dec, err := helpers.DecryptSecret(*d.PasswordEnc)
		if err != nil {
			return Config{}, fmt.Errorf("could not decrypt stored credential")
		}
		pw = dec
	}
	return Config{
		Engine:   d.Engine,
		Host:     d.Host,
		Port:     d.Port,
		Database: d.Database,
		Username: d.Username,
		Password: pw,
		SSLMode:  d.SSLMode,
	}, nil
}

// ───────────────────────── management (agent.manage gated at router) ─────────────────────────

// List returns the safe views of all data sources for the management surface,
// each flagged with whether this actor can manage it. (Route is agent.manage
// gated; this is the config/browse list, not the query-visibility list.)
func List(ctx context.Context, actor Actor) ([]View, error) {
	items, err := model.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load data sources")
	}
	out := make([]View, 0, len(items))
	for _, d := range items {
		out = append(out, toView(d, actor))
	}
	return out, nil
}

// Get returns one source's safe view if the actor may at least query it.
func Get(ctx context.Context, id uuid.UUID, actor Actor) (*View, error) {
	d, err := loadQueryable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	v := toView(d, actor)
	return &v, nil
}

// Create registers a new source owned by the actor.
func Create(ctx context.Context, in Input, actor Actor) (*View, error) {
	if err := validate(&in); err != nil {
		return nil, err
	}
	d := &model.DataSource{
		Name:       in.Name,
		Engine:     in.Engine,
		Host:       in.Host,
		Port:       in.Port,
		Database:   in.Database,
		Username:   in.Username,
		SSLMode:    in.SSLMode,
		Visibility: in.Visibility,
		Enabled:    true,
		CreatedBy:  actor.UserID,
	}
	if in.Enabled != nil {
		d.Enabled = *in.Enabled
	}
	if in.Password != nil && strings.TrimSpace(*in.Password) != "" {
		enc, err := helpers.EncryptSecret(*in.Password)
		if err != nil {
			return nil, fmt.Errorf("failed to secure credential")
		}
		d.PasswordEnc = &enc
	}
	id, err := model.Create(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("failed to create data source")
	}
	d.Id = id
	v := toView(d, actor)
	return &v, nil
}

// Update edits a source the actor manages. The password is only changed when a
// non-nil value is supplied (nil = leave unchanged; "" = clear).
func Update(ctx context.Context, id uuid.UUID, in Input, actor Actor) (*View, error) {
	d, err := loadManageable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	if err := validate(&in); err != nil {
		return nil, err
	}
	d.Name, d.Engine, d.Host, d.Port = in.Name, in.Engine, in.Host, in.Port
	d.Database, d.Username, d.SSLMode, d.Visibility = in.Database, in.Username, in.SSLMode, in.Visibility
	if in.Enabled != nil {
		d.Enabled = *in.Enabled
	}

	updatePassword := in.Password != nil
	if updatePassword {
		if strings.TrimSpace(*in.Password) == "" {
			d.PasswordEnc = nil
		} else {
			enc, encErr := helpers.EncryptSecret(*in.Password)
			if encErr != nil {
				return nil, fmt.Errorf("failed to secure credential")
			}
			d.PasswordEnc = &enc
		}
	}
	if err := model.Update(ctx, d, updatePassword); err != nil {
		return nil, fmt.Errorf("failed to update data source")
	}
	invalidateSchema(id) // host/creds/etc. may have changed; drop any cached schema
	v := toView(d, actor)
	return &v, nil
}

// SetEnabled pauses/resumes a source (manage-gated).
func SetEnabled(ctx context.Context, id uuid.UUID, enabled bool, actor Actor) error {
	if _, err := loadManageable(ctx, id, actor); err != nil {
		return err
	}
	if err := model.SetEnabled(ctx, id, enabled); err != nil {
		return fmt.Errorf("failed to update data source")
	}
	invalidateSchema(id)
	return nil
}

// Delete soft-deletes a source (manage-gated).
func Delete(ctx context.Context, id uuid.UUID, actor Actor) error {
	if _, err := loadManageable(ctx, id, actor); err != nil {
		return err
	}
	if err := model.SoftDelete(ctx, id); err != nil {
		return fmt.Errorf("failed to delete data source")
	}
	invalidateSchema(id)
	return nil
}

// ───────────────────────── connection use (read-only) ─────────────────────────

// TestConnection opens a read-only connection and pings it. Manage-gated: it is
// part of configuring a source and can reveal reachability of the host.
func TestConnection(ctx context.Context, id uuid.UUID, actor Actor) error {
	d, err := loadManageable(ctx, id, actor)
	if err != nil {
		return err
	}
	cfg, cerr := decryptConfig(d)
	if cerr != nil {
		return cerr
	}
	conn, nerr := NewConnector(cfg)
	if nerr != nil {
		return nerr
	}
	defer conn.Close()
	if perr := conn.Ping(ctx); perr != nil {
		return externalErr(opConnect, perr)
	}
	return nil
}

// Introspect returns the source's tables/columns. Query-gated (browsing the
// schema is a read/use of the source), so a member may introspect a source they
// can query but a private one they don't own is hidden.
func Introspect(ctx context.Context, id uuid.UUID, actor Actor) ([]TableSchema, error) {
	d, err := loadQueryable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	if !d.Enabled {
		return nil, fmt.Errorf("this data source is disabled")
	}
	cfg, cerr := decryptConfig(d)
	if cerr != nil {
		return nil, cerr
	}
	conn, nerr := NewConnector(cfg)
	if nerr != nil {
		return nil, nerr
	}
	defer conn.Close()
	return resolveSchema(ctx, id, conn)
}

// RunAggregate pushes a typed, deterministic, read-only aggregation down to the
// source and returns the result. Query-gated: enforced PER USER via visibility
// (the agent runs AS the user and is scoped identically), so a member can only
// aggregate a source they may query. Read-only by construction (see the
// connector); the caller never supplies SQL.
func RunAggregate(ctx context.Context, id uuid.UUID, spec AggSpec, actor Actor) (*AggResult, string, error) {
	d, err := loadQueryable(ctx, id, actor)
	if err != nil {
		return nil, "", err
	}
	if !d.Enabled {
		return nil, "", fmt.Errorf("this data source is disabled")
	}
	cfg, cerr := decryptConfig(d)
	if cerr != nil {
		return nil, "", cerr
	}
	conn, nerr := NewConnector(cfg)
	if nerr != nil {
		return nil, "", nerr
	}
	defer conn.Close()

	tables, terr := resolveSchema(ctx, id, conn)
	if terr != nil {
		return nil, "", terr
	}
	res, rerr := conn.RunAggregate(ctx, tables, spec)
	if rerr != nil {
		return nil, "", rerr
	}
	return res, d.Name, nil
}

// resolveSchema returns a source's introspected schema, from the short-TTL cache
// when warm (so a burst of agent queries doesn't re-read information_schema
// each time), else introspecting via the open connector and caching the result.
func resolveSchema(ctx context.Context, id uuid.UUID, conn Connector) ([]TableSchema, error) {
	if cached, ok := getCachedSchema(id); ok {
		return cached, nil
	}
	tables, err := conn.IntrospectTables(ctx)
	if err != nil {
		return nil, externalErr(opSchema, err)
	}
	setCachedSchema(id, tables)
	return tables, nil
}

// RunPlan pushes a typed, deterministic, read-only MULTI-STEP query plan down
// to the source and returns the result. Query-gated per user via visibility,
// exactly like RunAggregate. Read-only by construction; no SQL from the caller.
func RunPlan(ctx context.Context, id uuid.UUID, plan QueryPlan, actor Actor) (*PlanResult, string, error) {
	d, err := loadQueryable(ctx, id, actor)
	if err != nil {
		return nil, "", err
	}
	if !d.Enabled {
		return nil, "", fmt.Errorf("this data source is disabled")
	}
	cfg, cerr := decryptConfig(d)
	if cerr != nil {
		return nil, "", cerr
	}
	conn, nerr := NewConnector(cfg)
	if nerr != nil {
		return nil, "", nerr
	}
	defer conn.Close()

	tables, terr := resolveSchema(ctx, id, conn)
	if terr != nil {
		return nil, "", terr
	}
	res, rerr := conn.RunPlan(ctx, tables, plan)
	if rerr != nil {
		return nil, "", rerr
	}
	return res, d.Name, nil
}

// ListQueryable returns the enabled sources the actor may query, as safe views.
// It powers the agent's discovery of usable sources and the FE source picker;
// visibility is enforced per user (private = creator+admins, workspace = any).
func ListQueryable(ctx context.Context, actor Actor) ([]View, error) {
	items, err := model.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to load data sources")
	}
	out := make([]View, 0, len(items))
	for _, d := range items {
		if d.Enabled && canQuery(d, actor) {
			out = append(out, toView(d, actor))
		}
	}
	return out, nil
}

// ───────────────────────── loaders (permission-checked) ─────────────────────────

// loadQueryable loads a source the actor may query, or a forbidden/not-found error.
func loadQueryable(ctx context.Context, id uuid.UUID, actor Actor) (*model.DataSource, error) {
	d, err := model.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load data source")
	}
	if d == nil {
		return nil, errNotFound
	}
	if !canQuery(d, actor) {
		return nil, errForbidden
	}
	return d, nil
}

// CanQuery reports whether the actor may query a source, as a bare authority question:
// nil when they may, errNotFound / errForbidden / "disabled" otherwise.
//
// EXISTS FOR THE MCP AUTHORIZER, which has to decide authority BEFORE running a tool
// rather than discovering it from the tool's error. It is the same question every query
// operation here already asks — loadQueryable for the authority, then the Enabled check
// each one repeats — so the two cannot diverge: this delegates to both rather than
// restating either.
//
// ENABLED IS PART OF THE ANSWER, matching ListQueryable's definition of queryable rather
// than loadQueryable's. A disabled source is refused by every operation, so treating it
// as queryable and letting the tool fail would produce a refusal the authorizer could not
// explain and an audit row that read as a success followed by an error.
//
// Soft deletion needs no separate check: GetByID filters deleted_at, so a deleted source
// is indistinguishable from one that never existed — deliberately, since telling them
// apart would let a caller probe which sources exist.
func CanQuery(ctx context.Context, id uuid.UUID, actor Actor) error {
	d, err := loadQueryable(ctx, id, actor)
	if err != nil {
		return err
	}
	if !d.Enabled {
		return fmt.Errorf("this data source is disabled")
	}
	return nil
}

// loadManageable loads a source the actor may manage, or a forbidden/not-found error.
func loadManageable(ctx context.Context, id uuid.UUID, actor Actor) (*model.DataSource, error) {
	d, err := model.GetByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load data source")
	}
	if d == nil {
		return nil, errNotFound
	}
	if !canManage(d, actor) {
		return nil, errForbidden
	}
	return d, nil
}
