package business

// Management layer for MCP servers: validation, tool-prefix generation, CRUD
// for the server's creator or an admin (agent.manage-gated at the router),
// live connection testing/introspection, and registry rebuilds after every
// change.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	model "github.com/akashc777/OneCamp/models/postgres/AIMCP"
	"github.com/google/uuid"
)

const (
	maxNameLen        = 120
	maxDescriptionLen = 500
	maxURLLen         = 2000
)

// ServerInput is the create/update payload from the controller.
type ServerInput struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	URL            string `json:"url"`
	Transport      string `json:"transport"`
	AuthType       string `json:"auth_type"`
	AuthHeaderName string `json:"auth_header_name"`
	AuthSecret     string `json:"auth_secret"`
	Enabled        bool   `json:"enabled"`
}

// Actor is who is asking: their account and whether they are an admin.
type Actor struct {
	UserID  uuid.UUID
	IsAdmin bool
}

var (
	errNotFound  = fmt.Errorf("MCP server not found")
	errForbidden = fmt.Errorf("Only the person who added this server, or an admin, can change it.")
)

// IsNotFound lets the controller map to HTTP 404.
func IsNotFound(err error) bool { return err == errNotFound }

// IsForbidden lets the controller map to HTTP 403.
func IsForbidden(err error) bool { return err == errForbidden }

// manageable loads a server the actor may change: its creator, or an admin.
func manageable(ctx context.Context, id uuid.UUID, actor Actor) (*model.McpServer, error) {
	s, err := model.GetServerByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load MCP server")
	}
	if s == nil {
		return nil, errNotFound
	}
	if !s.ManageableBy(actor.UserID, actor.IsAdmin) {
		return nil, errForbidden
	}
	return s, nil
}

// validate cleans and checks an input, returning the secret-update intent
// (whether AuthSecret was provided) so the model can leave a stored secret
// unchanged on edit.
func validate(in *ServerInput, addedByAdmin bool) error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return fmt.Errorf("name is required")
	}
	if len(in.Name) > maxNameLen {
		return fmt.Errorf("name is too long")
	}
	if len(in.Description) > maxDescriptionLen {
		return fmt.Errorf("description is too long")
	}
	in.URL = strings.TrimSpace(in.URL)
	if in.URL == "" || len(in.URL) > maxURLLen {
		return fmt.Errorf("a valid url is required")
	}
	// The one address rule, checked now so a refused address is a sentence
	// on save rather than a failed introspection later. The client checks
	// again as it connects, which is the check that counts.
	if _, err := safefetch.CheckEndpoint(in.URL, safefetch.EndpointPolicy(addedByAdmin)); err != nil {
		if msg := helpers.EndpointRefusal(err, addedByAdmin); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return fmt.Errorf("url must be a valid http(s) endpoint")
	}

	if in.Transport == "" {
		in.Transport = model.TransportHTTP
	}
	if !model.ValidTransport(in.Transport) {
		return fmt.Errorf("unsupported transport")
	}

	if in.AuthType == "" {
		in.AuthType = model.AuthNone
	}
	if !model.ValidAuthType(in.AuthType) {
		return fmt.Errorf("invalid auth type")
	}
	in.AuthHeaderName = strings.TrimSpace(in.AuthHeaderName)
	if in.AuthType == model.AuthHeader && in.AuthHeaderName == "" {
		return fmt.Errorf("a header name is required for header auth")
	}
	if in.AuthType != model.AuthHeader {
		// Only header auth names a header; a stale name must not count as a
		// different destination for the secret.
		in.AuthHeaderName = ""
	}
	return nil
}

// slugify reduces a name to a lowercase alnum/underscore token for the prefix.
func slugify(name string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	s := strings.Trim(b.String(), "_")
	if s == "" {
		s = "server"
	}
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// uniqueToolPrefix builds a registry-safe, unique prefix for a server's tools,
// e.g. "mcp_github_". Collisions (rare; admin action) get a short random suffix.
func uniqueToolPrefix(ctx context.Context, name string, excludeID uuid.UUID) string {
	base := "mcp_" + slugify(name)
	existing, _ := model.ListServers(ctx)
	taken := map[string]bool{}
	for _, s := range existing {
		if s.Id != excludeID {
			taken[strings.TrimSuffix(s.ToolPrefix, "_")] = true
		}
	}
	candidate := base
	if taken[candidate] {
		suffix := make([]byte, 2)
		_, _ = rand.Read(suffix)
		candidate = base + "_" + hex.EncodeToString(suffix)
	}
	return candidate + "_"
}

// CreateServer validates, persists, then introspects + rebuilds the registry.
// The actor is its creator: an admin's server may be on this server's own
// network, a member's may not.
func CreateServer(ctx context.Context, in ServerInput, actor Actor) (*model.McpServer, error) {
	if err := validate(&in, actor.IsAdmin); err != nil {
		return nil, err
	}
	createdBy := actor.UserID
	m := &model.McpServer{
		Name:           in.Name,
		Description:    optStr(in.Description),
		URL:            in.URL,
		Transport:      in.Transport,
		AuthType:       in.AuthType,
		AuthHeaderName: optStr(in.AuthHeaderName),
		AuthSecret:     in.AuthSecret,
		Enabled:        in.Enabled,
		ToolPrefix:     uniqueToolPrefix(ctx, in.Name, uuid.Nil),
		CreatedBy:      &createdBy,
	}
	id, err := model.CreateServer(ctx, m)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP CreateServer err: %+v", err)
		return nil, fmt.Errorf("failed to create MCP server")
	}
	created, _ := model.GetServerByID(ctx, id)
	if created != nil {
		srv := created
		safeGo("introspect(create)", func() { introspectAndRebuild(context.WithoutCancel(ctx), srv) })
	}
	return created, nil
}

// UpdateServer validates and persists changes to a server the actor may
// manage; updateSecret controls whether the stored secret is replaced (so the
// UI can omit it on edit).
//
// A kept secret stays with its destination (helpers.CheckKeptSecret): moving
// the address, the auth type or the header name without entering the secret
// again is refused, and nothing is sent anywhere. Turning auth off drops the
// stored secret, since nothing would use it.
func UpdateServer(ctx context.Context, id uuid.UUID, in ServerInput, updateSecret bool, actor Actor) (*model.McpServer, error) {
	existing, err := manageable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	// What it may reach follows whoever added it, not whoever edits it.
	if err := validate(&in, creatorIsAdmin(ctx, existing.CreatedBy)); err != nil {
		return nil, err
	}
	if in.AuthType == model.AuthNone {
		updateSecret, in.AuthSecret = true, ""
	}
	// Replacing or clearing the secret keeps nothing, so only a kept one is
	// held to its destination.
	to := (&model.McpServer{URL: in.URL, AuthType: in.AuthType, AuthHeaderName: optStr(in.AuthHeaderName)}).SecretDestination()
	stored := existing.HasAuthSecret || existing.AuthSecretUnreadable
	if err := helpers.CheckKeptSecret(stored, updateSecret, existing.SecretDestination(), to, "token"); err != nil {
		return nil, err
	}
	existing.Name = in.Name
	existing.Description = optStr(in.Description)
	existing.URL = in.URL
	existing.Transport = in.Transport
	existing.AuthType = in.AuthType
	existing.AuthHeaderName = optStr(in.AuthHeaderName)
	existing.Enabled = in.Enabled
	if updateSecret {
		existing.AuthSecret = in.AuthSecret
	}
	// Keep the existing prefix (renames keep tool names stable) unless empty.
	if strings.TrimSpace(existing.ToolPrefix) == "" {
		existing.ToolPrefix = uniqueToolPrefix(ctx, in.Name, id)
	}
	if err := model.UpdateServer(ctx, existing, updateSecret); err != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP UpdateServer err: %+v", err)
		return nil, fmt.Errorf("failed to update MCP server")
	}
	updated, _ := model.GetServerByID(ctx, id)
	if updated != nil {
		srv := updated
		safeGo("introspect(update)", func() { introspectAndRebuild(context.WithoutCancel(ctx), srv) })
	}
	return updated, nil
}

// SetEnabled toggles a server the actor may manage and rebuilds the registry.
func SetEnabled(ctx context.Context, id uuid.UUID, enabled bool, actor Actor) error {
	if _, err := manageable(ctx, id, actor); err != nil {
		return err
	}
	if err := model.SetEnabled(ctx, id, enabled); err != nil {
		return err
	}
	safeGo("setEnabled", func() {
		bg := context.WithoutCancel(ctx)
		if enabled {
			if s, _ := model.GetServerByID(bg, id); s != nil {
				introspectAndRebuild(bg, s)
				return
			}
		}
		_ = RebuildRegistry(bg)
	})
	return nil
}

// DeleteServer soft-deletes a server the actor may manage and rebuilds the
// registry.
func DeleteServer(ctx context.Context, id uuid.UUID, actor Actor) error {
	if _, err := manageable(ctx, id, actor); err != nil {
		return err
	}
	if err := model.SoftDeleteServer(ctx, id); err != nil {
		return err
	}
	safeGo("delete", func() { _ = RebuildRegistry(context.WithoutCancel(ctx)) })
	return nil
}

// ListServers / GetServer pass through to the model.
func ListServers(ctx context.Context) ([]*model.McpServer, error) { return model.ListServers(ctx) }

func GetServer(ctx context.Context, id uuid.UUID) (*model.McpServer, error) {
	s, err := model.GetServerByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to load MCP server")
	}
	if s == nil {
		return nil, errNotFound
	}
	return s, nil
}

// TestConnection introspects a server live (initialize + tools/list), saves the
// result (tool cache or error), rebuilds the registry, and returns the tools so
// the admin sees them immediately.
//
// Only for the server's creator or an admin: a test sends the server's stored
// credential, and its answer lists what that credential can do.
func TestConnection(ctx context.Context, id uuid.UUID, actor Actor) ([]McpTool, error) {
	s, err := manageable(ctx, id, actor)
	if err != nil {
		return nil, err
	}
	tools, terr := introspect(ctx, s)
	if terr != nil {
		_ = model.SaveIntrospection(ctx, id, "", terr.Error())
		return nil, terr
	}
	saveIntrospection(ctx, id, tools)
	safeGo("testConnection", func() { _ = RebuildRegistry(context.WithoutCancel(ctx)) })
	return tools, nil
}

// introspect runs the live tools/list against a server.
func introspect(ctx context.Context, s *model.McpServer) ([]McpTool, error) {
	client, err := NewClient(ctx, s)
	if err != nil {
		// Not "could not connect": nothing was attempted, and wrapping it as a connection
		// failure would send an admin to check the remote instead of re-entering the secret.
		return nil, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		if msg := helpers.EndpointRefusal(err, creatorIsAdmin(ctx, s.CreatedBy)); msg != "" {
			return nil, fmt.Errorf("could not connect: %s", msg)
		}
		return nil, fmt.Errorf("could not connect: %w", err)
	}
	return tools, nil
}

// introspectAndRebuild introspects a server, persists the result, and rebuilds
// the registry. Used after create/update/enable; failures are logged + recorded
// (last_error) rather than surfaced, since these run in the background.
func introspectAndRebuild(ctx context.Context, s *model.McpServer) {
	tools, err := introspect(ctx, s)
	if err != nil {
		_ = model.SaveIntrospection(ctx, s.Id, "", err.Error())
		helpers.LogErrorWithContext(ctx, "AIMCP introspect server %s failed: %v", s.Id, err)
	} else {
		saveIntrospection(ctx, s.Id, tools)
	}
	if rerr := RebuildRegistry(ctx); rerr != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP rebuild after introspect err: %v", rerr)
	}
}

// saveIntrospection marshals + persists a tool list as the server's cache.
func saveIntrospection(ctx context.Context, id uuid.UUID, tools []McpTool) {
	blob, err := json.Marshal(tools)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP marshal tools err: %v", err)
		return
	}
	if serr := model.SaveIntrospection(ctx, id, string(blob), ""); serr != nil {
		helpers.LogErrorWithContext(ctx, "AIMCP save introspection err: %v", serr)
	}
}

func optStr(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// safeGo runs a background task with panic recovery so a failure in
// introspection or a registry rebuild can never crash the process. These run
// detached from the request, so a panic would otherwise be unrecovered.
func safeGo(label string, fn func()) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				helpers.MessageLogs.ErrorLog.Printf("AIMCP %s: recovered panic: %v", label, rec)
			}
		}()
		fn()
	}()
}
