package business

// connector.go — the READ-ONLY external-database connector used by data
// sources. It is ENGINE-GENERIC by construction:
//
//   - a small Connector interface (Ping / IntrospectTables / RunAggregate /
//     RunPlan) is the whole surface the rest of the app depends on;
//   - a Dialect captures the only things that differ between SQL engines
//     (identifier quoting, placeholder style, type casts, the day-truncation and
//     case-insensitive-contains expressions, the introspection query, and type
//     normalization), so the aggregate/plan SQL builders are written ONCE and
//     work for every engine;
//   - an engine REGISTRY maps an engine name to a factory, so adding an engine
//     (mysql, snowflake, redshift, …) is a pure code change: implement a Dialect
//     + a factory that opens the driver, and register it. No migration, no
//     changes to the builders, the business layer, or the agent tools.
//
// Every path is defensively read-only: connections open with a short connect
// timeout + tiny pool, every statement runs inside a READ-ONLY transaction with
// a per-statement timeout, and the caller only ever hands us a typed spec/plan
// — never free-form SQL — so there is no injection surface from the model.
//
// Credentials live only in the in-memory Config (decrypted by the business
// layer just before use); nothing here logs or returns them.
//
// Non-SQL engines (e.g. BigQuery, which is an HTTP API with named parameters and
// service-account auth rather than a database/sql DSN) are intentionally NOT
// forced through the Dialect path — they can implement the Connector interface
// directly and register their own factory, reusing everything above them.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/lib/pq"

	model "github.com/akashc777/OneCamp/models/postgres/DataSource"
)

// Config is the decrypted, in-memory connection config. It is never persisted
// or logged from this package.
type Config struct {
	Engine   string
	Host     string
	Port     int
	Database string
	Username string
	Password string
	SSLMode  string
}

// ColumnSchema describes one external column, with its type normalized to the
// small set the aggregation layer understands (so a plan can reason about which
// columns are numeric / temporal regardless of engine).
type ColumnSchema struct {
	Name       string `json:"name"`
	DataType   string `json:"data_type"`   // normalized: text|number|date|boolean|other
	NativeType string `json:"native_type"` // the engine's own type name (for display)
	Nullable   bool   `json:"nullable"`
}

// TableSchema describes one external table/view the source exposes.
type TableSchema struct {
	Schema  string         `json:"schema"`
	Name    string         `json:"name"`
	Columns []ColumnSchema `json:"columns"`
}

// Connector is the engine-generic, read-only surface over an external database.
type Connector interface {
	// Ping validates that the connection works (used by "Test connection").
	Ping(ctx context.Context) error
	// IntrospectTables lists the tables/views + columns the source exposes,
	// excluding the engine's system schemas.
	IntrospectTables(ctx context.Context) ([]TableSchema, error)
	// RunAggregate pushes a typed, deterministic aggregation down to the engine
	// as safe, parameterized read-only SQL. The caller supplies the resolved
	// schema (so introspection can be cached); identifiers are validated
	// against it. The model never writes SQL.
	RunAggregate(ctx context.Context, tables []TableSchema, spec AggSpec) (*AggResult, error)
	// RunPlan pushes a MULTI-STEP query plan (several metrics, having,
	// share-of-total, sort, limit) down as one parameterized read-only statement.
	RunPlan(ctx context.Context, tables []TableSchema, plan QueryPlan) (*PlanResult, error)
	// Close releases the underlying pool.
	Close() error
}

// Dialect captures the per-engine SQL differences so the aggregate/plan builders
// are written once. All returned fragments are composed with already-quoted
// identifiers and bound placeholders, never raw values.
type Dialect interface {
	// QuoteIdent safely quotes an identifier (schema/table/column).
	QuoteIdent(name string) string
	// Placeholder renders the 1-based bound-parameter placeholder ($1 vs ?).
	Placeholder(oneBasedIndex int) string
	// CastText renders expr as text ((expr)::text vs CAST(expr AS CHAR)).
	CastText(expr string) string
	// CastNumericParam / CastDateParam wrap a placeholder so a bound (text)
	// value compares correctly against a numeric / temporal column.
	CastNumericParam(placeholder string) string
	CastDateParam(placeholder string) string
	// DateTruncDay renders a day-granularity bucket for a temporal column.
	DateTruncDay(quotedCol string) string
	// ContainsExpr renders a case-insensitive substring match of a bound value.
	ContainsExpr(textExpr, placeholder string) string
	// TimeoutStmt returns a statement that bounds per-statement runtime (or "").
	TimeoutStmt(ms int) string
	// IntrospectQuery returns SQL selecting (schema, name, column, data_type,
	// is_nullable) for non-system tables/views, bounded by maxCols.
	IntrospectQuery(maxCols int) string
	// NormalizeType maps an engine native type to text|number|date|boolean|other.
	NormalizeType(nativeType string) string
}

// connect timeout / statement timeout / bounds shared by all engines.
const (
	connectTimeoutSecs   = 5
	statementTimeoutMS   = 15000
	maxIntrospectTables  = 500
	maxIntrospectColumns = 2000
	pingTimeout          = 6 * time.Second
	introspectTimeout    = 20 * time.Second
)

// engineFactory opens a ready (but lazily-connected) Connector for a config.
type engineFactory func(cfg Config) (Connector, error)

// engineFactories is the registry. Adding an engine = register a factory here.
var engineFactories = map[string]engineFactory{
	"postgres": newPostgresConnector,
	"mysql":    newMySQLConnector,
}

// NewConnector builds a read-only connector for the engine in cfg (defaulting to
// postgres). It opens the pool but does not connect until first use.
func NewConnector(cfg Config) (Connector, error) {
	engine := cfg.Engine
	if engine == "" {
		engine = "postgres"
	}
	f, ok := engineFactories[engine]
	if !ok {
		return nil, fmt.Errorf("unsupported engine %q", cfg.Engine)
	}
	return f(cfg)
}

// ───────────────────────── generic SQL connector ─────────────────────────

// sqlConnector is the database/sql-backed, dialect-driven Connector shared by
// every SQL engine. All engine differences live in its Dialect.
type sqlConnector struct {
	db      *sql.DB
	dialect Dialect
}

func (c *sqlConnector) Close() error { return c.db.Close() }

func (c *sqlConnector) Ping(ctx context.Context) error {
	cctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	// Validate through a read-only transaction so a misconfigured, write-only
	// account still surfaces as reachable (and we prove read-only works).
	tx, err := c.db.BeginTx(cctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx, nothing to commit
	var one int
	if err := tx.QueryRowContext(cctx, "SELECT 1").Scan(&one); err != nil {
		return err
	}
	return nil
}

func (c *sqlConnector) IntrospectTables(ctx context.Context) ([]TableSchema, error) {
	cctx, cancel := context.WithTimeout(ctx, introspectTimeout)
	defer cancel()

	tx, err := c.db.BeginTx(cctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx
	if stmt := c.dialect.TimeoutStmt(statementTimeoutMS); stmt != "" {
		if _, err := tx.ExecContext(cctx, stmt); err != nil {
			return nil, err
		}
	}

	rows, err := tx.QueryContext(cctx, c.dialect.IntrospectQuery(maxIntrospectColumns))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Group columns by (schema,name) preserving first-seen table order.
	type key struct{ schema, name string }
	byTable := map[key]*TableSchema{}
	var order []key
	for rows.Next() {
		var schema, name, col, dataType, isNullable string
		if err := rows.Scan(&schema, &name, &col, &dataType, &isNullable); err != nil {
			return nil, err
		}
		k := key{schema, name}
		ts := byTable[k]
		if ts == nil {
			if len(order) >= maxIntrospectTables {
				continue
			}
			ts = &TableSchema{Schema: schema, Name: name}
			byTable[k] = ts
			order = append(order, k)
		}
		ts.Columns = append(ts.Columns, ColumnSchema{
			Name:       col,
			DataType:   c.dialect.NormalizeType(dataType),
			NativeType: dataType,
			Nullable:   strings.EqualFold(isNullable, "YES"),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]TableSchema, 0, len(order))
	for _, k := range order {
		out = append(out, *byTable[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Schema != out[j].Schema {
			return out[i].Schema < out[j].Schema
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ───────────────────────── Postgres ─────────────────────────

type postgresDialect struct{}

func (postgresDialect) QuoteIdent(name string) string    { return pq.QuoteIdentifier(name) }
func (postgresDialect) Placeholder(i int) string         { return "$" + strconv.Itoa(i) }
func (postgresDialect) CastText(expr string) string      { return "(" + expr + ")::text" }
func (postgresDialect) CastNumericParam(p string) string { return p + "::numeric" }
func (postgresDialect) CastDateParam(p string) string    { return p + "::timestamptz" }
func (postgresDialect) DateTruncDay(col string) string   { return "date_trunc('day', " + col + ")::date" }
func (postgresDialect) ContainsExpr(textExpr, p string) string {
	return textExpr + " ILIKE '%' || " + p + " || '%'"
}
func (postgresDialect) TimeoutStmt(ms int) string {
	return "SET LOCAL statement_timeout = " + strconv.Itoa(ms)
}
func (postgresDialect) IntrospectQuery(maxCols int) string {
	return `
SELECT c.table_schema, c.table_name, c.column_name, c.data_type, c.is_nullable
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema NOT IN ('pg_catalog','information_schema')
  AND c.table_schema NOT LIKE 'pg_toast%'
  AND t.table_type IN ('BASE TABLE','VIEW')
ORDER BY c.table_schema, c.table_name, c.ordinal_position
LIMIT ` + strconv.Itoa(maxCols)
}
func (postgresDialect) NormalizeType(t string) string {
	switch t {
	case "smallint", "integer", "bigint", "decimal", "numeric", "real",
		"double precision", "money":
		return "number"
	case "boolean":
		return "boolean"
	case "date", "timestamp", "timestamp without time zone",
		"timestamp with time zone", "time", "time without time zone",
		"time with time zone":
		return "date"
	case "character varying", "varchar", "character", "char", "text",
		"uuid", "name", "citext":
		return "text"
	default:
		return "other"
	}
}

func newPostgresConnector(cfg Config) (Connector, error) {
	if cfg.Host == "" || cfg.Database == "" {
		return nil, fmt.Errorf("host and database are required")
	}
	port := cfg.Port
	if port == 0 {
		port = 5432
	}
	sslMode := cfg.SSLMode
	if sslMode == "" {
		sslMode = "require"
	}
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(cfg.Username, cfg.Password),
		Host:   net.JoinHostPort(cfg.Host, strconv.Itoa(port)),
		Path:   "/" + cfg.Database,
	}
	q := url.Values{}
	q.Set("sslmode", sslMode)
	q.Set("connect_timeout", strconv.Itoa(connectTimeoutSecs))
	// Belt-and-suspenders: make the whole session read-only, on top of the
	// per-statement read-only transaction we open for each query.
	q.Set("options", "-c default_transaction_read_only=on")
	u.RawQuery = q.Encode()

	// Open through pq's Connector so we can install the guarded dialer: the
	// destination policy is re-checked at dial time and we connect to the exact
	// address we validated. sslmode semantics are untouched — pq keeps deriving
	// TLS from sslmode, and verify-full still verifies against the DSN host name
	// (not the pinned IP), so pinning does not weaken certificate checks.
	pgConnector, err := pq.NewConnector(u.String())
	if err != nil {
		return nil, externalErr(opConnect, err)
	}
	pgConnector.Dialer(pqGuardedDialer{})
	db := sql.OpenDB(pgConnector)
	tunePool(db)
	return &sqlConnector{db: db, dialect: postgresDialect{}}, nil
}

// ───────────────────────── MySQL ─────────────────────────

type mysqlDialect struct{}

func (mysqlDialect) QuoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
func (mysqlDialect) Placeholder(int) string           { return "?" }
func (mysqlDialect) CastText(expr string) string      { return "CAST(" + expr + " AS CHAR)" }
func (mysqlDialect) CastNumericParam(p string) string { return "CAST(" + p + " AS DECIMAL(65,10))" }
func (mysqlDialect) CastDateParam(p string) string    { return "CAST(" + p + " AS DATETIME)" }
func (mysqlDialect) DateTruncDay(col string) string   { return "DATE(" + col + ")" }
func (mysqlDialect) ContainsExpr(textExpr, p string) string {
	return "LOWER(" + textExpr + ") LIKE CONCAT('%', LOWER(" + p + "), '%')"
}
func (mysqlDialect) TimeoutStmt(ms int) string {
	// Session-scoped; MySQL enforces it for read-only SELECTs (which is all we run).
	return "SET SESSION max_execution_time = " + strconv.Itoa(ms)
}
func (mysqlDialect) IntrospectQuery(maxCols int) string {
	return `
SELECT c.table_schema, c.table_name, c.column_name, c.data_type, c.is_nullable
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema NOT IN ('mysql','information_schema','performance_schema','sys')
  AND t.table_type IN ('BASE TABLE','VIEW')
ORDER BY c.table_schema, c.table_name, c.ordinal_position
LIMIT ` + strconv.Itoa(maxCols)
}
func (mysqlDialect) NormalizeType(t string) string {
	switch strings.ToLower(t) {
	case "tinyint", "smallint", "mediumint", "int", "integer", "bigint",
		"decimal", "numeric", "float", "double", "dec", "fixed", "bit":
		return "number"
	case "bool", "boolean":
		return "boolean"
	case "date", "datetime", "timestamp", "time", "year":
		return "date"
	case "char", "varchar", "text", "tinytext", "mediumtext", "longtext",
		"enum", "set":
		return "text"
	default:
		return "other"
	}
}

func newMySQLConnector(cfg Config) (Connector, error) {
	if cfg.Host == "" || cfg.Database == "" {
		return nil, fmt.Errorf("host and database are required")
	}
	port := cfg.Port
	if port == 0 {
		port = 3306
	}
	mc := gomysql.NewConfig()
	mc.User = cfg.Username
	mc.Passwd = cfg.Password
	// Dial through the guarded network registered above so the destination policy
	// is re-applied at connect time (DNS rebinding) for MySQL too.
	mc.Net = mysqlGuardedNet
	mc.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	mc.DBName = cfg.Database
	mc.ParseTime = true
	mc.Timeout = connectTimeoutSecs * time.Second
	mc.ReadTimeout = introspectTimeout
	tlsConf, terr := mysqlTLS(cfg.SSLMode, cfg.Host)
	if terr != nil {
		return nil, terr
	}
	// Set the *tls.Config directly (via the driver's Connector) instead of a
	// name in the driver's global TLS registry: the config is per-source (its
	// ServerName is this host) and never leaks into a DSN string.
	mc.TLS = tlsConf

	myConnector, err := gomysql.NewConnector(mc)
	if err != nil {
		return nil, externalErr(opConnect, err)
	}
	db := sql.OpenDB(myConnector)
	tunePool(db)
	return &sqlConnector{db: db, dialect: mysqlDialect{}}, nil
}

// mysqlGuardedNet is the custom network name our guarded dialer is registered
// under; MySQL has no DSN-level dialer hook, only this registry.
const mysqlGuardedNet = "onecamp-datasource-tcp"

func init() {
	gomysql.RegisterDialContext(mysqlGuardedNet, func(ctx context.Context, addr string) (net.Conn, error) {
		return guardedDialContext(ctx, "tcp", addr)
	})
}

// mysqlTLS builds the real TLS config for our libpq-style ssl_mode. MySQL's
// driver shorthands cannot express these three modes honestly ("skip-verify"
// encrypts without authenticating the server; "true" always checks the hostname),
// so each mode gets a config that does exactly what its name promises:
//
//	disable     : no TLS.
//	require     : encrypt, do not authenticate the server (libpq's semantics).
//	verify-ca   : verify the certificate CHAIN against the system roots, but not
//	              the hostname (so an internal cert issued for another name still
//	              works, which is why an operator picks verify-ca).
//	verify-full : verify the chain AND that the certificate is for this host.
//
// An unknown mode is an error rather than a silent downgrade.
func mysqlTLS(sslMode, host string) (*tls.Config, error) {
	switch sslMode {
	case model.SSLDisable:
		return nil, nil
	case model.SSLRequire, "":
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Encryption only: libpq's "require" does not authenticate the
			// server either. Pick verify-ca/verify-full to authenticate it.
			InsecureSkipVerify: true, //nolint:gosec // documented ssl_mode=require semantics
		}, nil
	case model.SSLVerifyCA:
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			// Chain verified by VerifyPeerCertificate below; Go's built-in
			// verification cannot skip the hostname check on its own.
			InsecureSkipVerify:    true, //nolint:gosec // chain verified in VerifyPeerCertificate
			VerifyPeerCertificate: verifyChainOnly,
		}, nil
	case model.SSLVerifyFull:
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			// ServerName pinned to the CONFIGURED host so hostname verification
			// still holds when the guarded dialer connects to a validated IP.
			ServerName: host,
		}, nil
	default:
		return nil, fmt.Errorf("invalid ssl_mode %q", sslMode)
	}
}

// verifyChainOnly verifies the server's certificate chain against the system
// roots while deliberately NOT checking the hostname (ssl_mode=verify-ca).
func verifyChainOnly(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("tls: server presented no certificate")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("tls: could not parse server certificate: %w", err)
		}
		certs = append(certs, c)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fmt.Errorf("tls: could not load system CA pool: %w", err)
	}
	intermediates := x509.NewCertPool()
	for _, c := range certs[1:] {
		intermediates.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("tls: server certificate chain is not trusted: %w", err)
	}
	return nil
}

// tunePool applies the shared, deliberately-tiny pool settings: a data source is
// queried occasionally, not hammered, so we never hold many external connections.
func tunePool(db *sql.DB) {
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(2 * time.Minute)
	db.SetConnMaxIdleTime(30 * time.Second)
}
