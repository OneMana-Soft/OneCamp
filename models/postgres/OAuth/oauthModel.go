// Package models (OAuth) stores the OAuth 2.1 state behind the MCP endpoint:
// registered clients, sign-ins waiting for approval, one-time codes and live
// grants (migration 164). Secrets are stored only as hashes.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Client is a self-registered public client.
type Client struct {
	ID           uuid.UUID
	Name         string
	RedirectURIs []string
}

// Request is a sign-in waiting for the person to approve it.
type Request struct {
	ID            uuid.UUID
	ClientID      uuid.UUID
	RedirectURI   string
	State         string
	Scopes        []string
	CodeChallenge string
	ExpiresAt     time.Time
}

// Code is an approved sign-in, exchangeable once.
type Code struct {
	Hash          string
	ClientID      uuid.UUID
	UserID        uuid.UUID
	AgentID       uuid.UUID
	Scopes        []string
	RedirectURI   string
	CodeChallenge string
	ExpiresAt     time.Time
}

// Grant is a live connection.
type Grant struct {
	ID               uuid.UUID
	ClientID         uuid.UUID
	UserID           uuid.UUID
	AgentID          uuid.UUID
	TokenID          uuid.UUID
	RefreshExpiresAt time.Time
}

func db() *sql.DB { return postgresInit.DBConn.SqlDB }

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

func strList(raw []byte) []string {
	var out []string
	_ = json.Unmarshal(raw, &out)
	return out
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// CreateClient registers a client and returns its id, which is its client_id.
func CreateClient(ctx context.Context, name string, redirectURIs []string) (uuid.UUID, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	var id uuid.UUID
	err := db().QueryRowContext(c,
		`INSERT INTO oauth_clients (client_name, redirect_uris) VALUES ($1, $2) RETURNING id`,
		name, jsonList(redirectURIs)).Scan(&id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/OAuth CreateClient err: %+v", err)
	}
	return id, err
}

// GetClient returns a client, or nil when there is none.
func GetClient(ctx context.Context, id uuid.UUID) (*Client, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	cl := &Client{ID: id}
	var uris []byte
	err := db().QueryRowContext(c,
		`SELECT client_name, redirect_uris FROM oauth_clients WHERE id=$1`, id).Scan(&cl.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cl.RedirectURIs = strList(uris)
	return cl, nil
}

// prune drops expired requests and codes. Called on insert, so the tables stay
// the size of the sign-ins in flight without a sweeper.
func prune(ctx context.Context) {
	_, _ = db().ExecContext(ctx, `DELETE FROM oauth_requests WHERE expires_at < NOW()`)
	_, _ = db().ExecContext(ctx, `DELETE FROM oauth_codes WHERE expires_at < NOW()`)
}

// CreateRequest stores a sign-in awaiting approval and returns its id.
func CreateRequest(ctx context.Context, r *Request) (uuid.UUID, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	prune(c)
	var id uuid.UUID
	err := db().QueryRowContext(c,
		`INSERT INTO oauth_requests (client_id, redirect_uri, state, scopes, code_challenge, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		r.ClientID, r.RedirectURI, r.State, jsonList(r.Scopes), r.CodeChallenge, r.ExpiresAt).Scan(&id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/OAuth CreateRequest err: %+v", err)
	}
	return id, err
}

const requestColumns = `id, client_id, redirect_uri, state, scopes, code_challenge, expires_at`

func scanRequest(row *sql.Row) (*Request, error) {
	r := &Request{}
	var scopes []byte
	err := row.Scan(&r.ID, &r.ClientID, &r.RedirectURI, &r.State, &scopes, &r.CodeChallenge, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Scopes = strList(scopes)
	return r, nil
}

// GetRequest returns an unexpired pending sign-in, or nil.
func GetRequest(ctx context.Context, id uuid.UUID) (*Request, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	return scanRequest(db().QueryRowContext(c,
		`SELECT `+requestColumns+` FROM oauth_requests WHERE id=$1 AND expires_at > NOW()`, id))
}

// TakeRequest removes and returns an unexpired pending sign-in, so it is
// answered once, whichever way.
func TakeRequest(ctx context.Context, id uuid.UUID) (*Request, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	return scanRequest(db().QueryRowContext(c,
		`DELETE FROM oauth_requests WHERE id=$1 AND expires_at > NOW() RETURNING `+requestColumns, id))
}

// CreateCode stores an approved sign-in's one-time code (hash only).
func CreateCode(ctx context.Context, code *Code) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(c,
		`INSERT INTO oauth_codes (code_hash, client_id, user_id, agent_id, scopes, redirect_uri, code_challenge, expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		code.Hash, code.ClientID, code.UserID, code.AgentID, jsonList(code.Scopes), code.RedirectURI, code.CodeChallenge, code.ExpiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/OAuth CreateCode err: %+v", err)
	}
	return err
}

// TakeCode removes and returns an unexpired code, or nil. Deleting on read is
// what makes a code single-use even when two exchanges race.
func TakeCode(ctx context.Context, hash string) (*Code, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	code := &Code{Hash: hash}
	var scopes []byte
	err := db().QueryRowContext(c,
		`DELETE FROM oauth_codes WHERE code_hash=$1 AND expires_at > NOW()
		 RETURNING client_id, user_id, agent_id, scopes, redirect_uri, code_challenge, expires_at`, hash).
		Scan(&code.ClientID, &code.UserID, &code.AgentID, &scopes, &code.RedirectURI, &code.CodeChallenge, &code.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	code.Scopes = strList(scopes)
	return code, nil
}

// CreateGrant records a live connection.
func CreateGrant(ctx context.Context, g *Grant, refreshHash string) error {
	c, cancel := withTimeout(ctx)
	defer cancel()
	_, err := db().ExecContext(c,
		`INSERT INTO oauth_grants (client_id, user_id, agent_id, token_id, refresh_hash, refresh_expires_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		g.ClientID, g.UserID, g.AgentID, g.TokenID, refreshHash, g.RefreshExpiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/OAuth CreateGrant err: %+v", err)
	}
	_, _ = db().ExecContext(c, `UPDATE oauth_clients SET last_used_at=NOW() WHERE id=$1`, g.ClientID)
	return err
}

// GrantByRefresh returns the live grant a refresh token belongs to, or nil.
func GrantByRefresh(ctx context.Context, refreshHash string) (*Grant, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	g := &Grant{}
	err := db().QueryRowContext(c,
		`SELECT id, client_id, user_id, agent_id, token_id, refresh_expires_at FROM oauth_grants
		 WHERE refresh_hash=$1 AND revoked_at IS NULL AND refresh_expires_at > NOW()`, refreshHash).
		Scan(&g.ID, &g.ClientID, &g.UserID, &g.AgentID, &g.TokenID, &g.RefreshExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// RotateRefresh swaps a grant's refresh token, only if it still holds the one
// presented. Two refreshes racing with the same token: one wins, the other
// gets false and is refused.
func RotateRefresh(ctx context.Context, id uuid.UUID, oldHash, newHash string, expiresAt time.Time) (bool, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	res, err := db().ExecContext(c,
		`UPDATE oauth_grants SET refresh_hash=$3, refresh_expires_at=$4
		 WHERE id=$1 AND refresh_hash=$2 AND revoked_at IS NULL`, id, oldHash, newHash, expiresAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RevokeGrant ends a grant and returns the credential it renewed, so the
// caller can revoke that too. Nil when there was nothing live.
func RevokeGrant(ctx context.Context, id uuid.UUID) (*uuid.UUID, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	var tokenID uuid.UUID
	err := db().QueryRowContext(c,
		`UPDATE oauth_grants SET revoked_at=NOW() WHERE id=$1 AND revoked_at IS NULL RETURNING token_id`, id).Scan(&tokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tokenID, nil
}

// GrantByToken returns the live grant renewing a credential, or nil.
func GrantByToken(ctx context.Context, tokenID uuid.UUID) (*Grant, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	g := &Grant{}
	err := db().QueryRowContext(c,
		`SELECT id, client_id, user_id, agent_id, token_id, refresh_expires_at FROM oauth_grants
		 WHERE token_id=$1 AND revoked_at IS NULL`, tokenID).
		Scan(&g.ID, &g.ClientID, &g.UserID, &g.AgentID, &g.TokenID, &g.RefreshExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return g, nil
}

// LiveGrantsFor returns the live grants a person has for an agent from clients
// with a given name: the earlier connections a reconnect replaces.
func LiveGrantsFor(ctx context.Context, userID, agentID uuid.UUID, clientName string) ([]uuid.UUID, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(c,
		`SELECT g.id FROM oauth_grants g JOIN oauth_clients cl ON cl.id = g.client_id
		 WHERE g.user_id=$1 AND g.agent_id=$2 AND cl.client_name=$3 AND g.revoked_at IS NULL`,
		userID, agentID, clientName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Connection is one live outside-agent connection as its owner sees it: which
// app, acting as which agent, allowed what, and when it last did anything.
type Connection struct {
	ID         uuid.UUID  `json:"id"`
	ClientName string     `json:"client_name"`
	AgentID    uuid.UUID  `json:"agent_id"`
	AgentName  string     `json:"agent_name"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ConnectionsFor lists a person's live connections, newest first. An agent
// deleted since shows with an empty name rather than hiding the connection.
func ConnectionsFor(ctx context.Context, userID uuid.UUID) ([]Connection, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(c,
		`SELECT g.id, cl.client_name, g.agent_id, COALESCE(a.name, ''), t.scopes, g.created_at, t.last_used_at
		   FROM oauth_grants g
		   JOIN oauth_clients cl ON cl.id = g.client_id
		   JOIN api_tokens t ON t.id = g.token_id
		   LEFT JOIN ai_agents a ON a.id = g.agent_id AND a.deleted_at IS NULL
		  WHERE g.user_id=$1 AND g.revoked_at IS NULL
		  ORDER BY g.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Connection{}
	for rows.Next() {
		var k Connection
		var scopes []byte
		if err := rows.Scan(&k.ID, &k.ClientName, &k.AgentID, &k.AgentName, &scopes, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return nil, err
		}
		if k.Scopes = strList(scopes); k.Scopes == nil {
			k.Scopes = []string{}
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GrantOwner returns who a live grant belongs to, or uuid.Nil when there is no
// such live grant.
func GrantOwner(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
	c, cancel := withTimeout(ctx)
	defer cancel()
	var owner uuid.UUID
	err := db().QueryRowContext(c, `SELECT user_id FROM oauth_grants WHERE id=$1 AND revoked_at IS NULL`, id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil
	}
	return owner, err
}
