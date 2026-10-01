package business

// connError.go — the ERROR SURFACE for external data sources.
//
// Two audiences, two strings:
//
//   - the SERVER LOG gets the full wrapped error (driver text, SQLSTATE, DSN
//     fragments, hostnames, SQL) because that is what an operator debugs with;
//   - the CLIENT gets a stable, sanitized sentence from PublicMessage. It never
//     contains driver internals, and it does not change when the driver's
//     wording changes, so the UI can match on it.
//
// Anything that talks to the external database wraps its failure in
// externalError (with what we were doing: connect / schema / query). Errors NOT
// wrapped that way are messages this package authored — validation, permission,
// destination policy — and are already safe to show verbatim, so they pass
// through unchanged and stay as useful as they are today.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

// what we were doing when the external database failed us.
const (
	opConnect = "connect"
	opSchema  = "schema"
	opQuery   = "query"
)

// externalError marks a failure that came from the external database, its driver
// or the network. Error() keeps every detail (for the log); the public wording is
// derived by PublicMessage.
type externalError struct {
	op  string
	err error
}

func (e *externalError) Error() string { return "data source " + e.op + ": " + e.err.Error() }
func (e *externalError) Unwrap() error { return e.err }

// externalErr wraps err as an external failure for the given op.
func externalErr(op string, err error) error {
	if err == nil {
		return nil
	}
	return &externalError{op: op, err: err}
}

// Stable public messages. Admin-actionable, engine-neutral, no driver text.
const (
	msgUnreachable   = "could not reach the database host on that port"
	msgRefusedByDB   = "the database server refused a connection from this server"
	msgTimeout       = "the database did not respond in time"
	msgAuthFailed    = "the database rejected the username or password"
	msgUnknownDB     = "the database name does not exist on that server"
	msgNoPermission  = "the database user is not allowed to read that data"
	msgTLSFailed     = "the encrypted (TLS) connection failed — check ssl_mode and the server's certificate"
	msgConnectFailed = "could not connect to the database"
	msgSchemaFailed  = "could not read the database schema"
	msgQueryFailed   = "the database could not run that query"
)

// PublicMessage returns the sanitized message a client may see for err, plus
// whether err was an EXTERNAL failure — when true the caller must log the full
// error server-side, because the returned string deliberately drops the detail.
func PublicMessage(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	// Destination-policy refusals are our own wording and the most useful thing
	// an operator can be told, even when a driver handed it back from a dial.
	var de *destinationError
	if errors.As(err, &de) {
		return de.msg, false
	}
	var ee *externalError
	if !errors.As(err, &ee) {
		// Authored validation / permission / config message: already safe.
		return err.Error(), false
	}
	return classifyExternal(ee), true
}

// classifyExternal picks the most specific stable message the failure supports,
// falling back to a per-operation generic one.
func classifyExternal(ee *externalError) string {
	err := ee.err
	switch {
	case isTLSFailure(err):
		return msgTLSFailed
	case isAuthFailure(err):
		return msgAuthFailed
	case isUnknownDatabase(err):
		return msgUnknownDB
	case isPermissionFailure(err):
		return msgNoPermission
	case isConnectionRefusedByServer(err):
		return msgRefusedByDB
	case isTimeout(err):
		return msgTimeout
	case isUnreachable(err):
		return msgUnreachable
	}
	switch ee.op {
	case opSchema:
		return msgSchemaFailed
	case opQuery:
		return msgQueryFailed
	default:
		return msgConnectFailed
	}
}

// pgCode returns the SQLSTATE of a Postgres server error ("" when not one).
func pgCode(err error) string {
	var pe *pq.Error
	if errors.As(err, &pe) {
		return string(pe.Code)
	}
	return ""
}

// myNumber returns the MySQL server error number (0 when not one).
func myNumber(err error) uint16 {
	var me *gomysql.MySQLError
	if errors.As(err, &me) {
		return me.Number
	}
	return 0
}

// containsAny matches driver wording only to CLASSIFY; the text itself is never
// returned to a client.
func containsAny(err error, needles ...string) bool {
	s := strings.ToLower(err.Error())
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func isTLSFailure(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var recordHeader tls.RecordHeaderError
	var certVerify *tls.CertificateVerificationError
	switch {
	case errors.As(err, &unknownAuthority), errors.As(err, &hostnameErr),
		errors.As(err, &certInvalid), errors.As(err, &recordHeader),
		errors.As(err, &certVerify):
		return true
	case errors.Is(err, gomysql.ErrNoTLS):
		return true
	}
	// pq reports a server without SSL as a plain error; both drivers surface
	// handshake problems as "tls: ..." / x509 text.
	return containsAny(err, "tls:", "x509", "certificate", "ssl is not enabled", "ssl connection")
}

func isAuthFailure(err error) bool {
	switch pgCode(err) {
	case "28P01", "28000": // invalid_password, invalid_authorization_specification
		return true
	}
	switch myNumber(err) {
	case 1045, 1251, 1698: // access denied, unsupported auth protocol, auth plugin
		return true
	}
	return false
}

func isUnknownDatabase(err error) bool {
	if pgCode(err) == "3D000" { // invalid_catalog_name
		return true
	}
	return myNumber(err) == 1049 // unknown database
}

func isPermissionFailure(err error) bool {
	switch pgCode(err) {
	case "42501", "42P01": // insufficient_privilege, undefined_table (hidden by grants)
		return true
	}
	switch myNumber(err) {
	case 1044, 1142, 1143, 1146: // db access denied, table/column denied, unknown table
		return true
	}
	return false
}

// isConnectionRefusedByServer covers the server ACCEPTING the TCP connection and
// then refusing this client (host not allowed / too many connections), which is a
// different fix for an admin than an unreachable host.
func isConnectionRefusedByServer(err error) bool {
	switch myNumber(err) {
	case 1130, 1040, 1129: // host not allowed, too many connections, host blocked
		return true
	}
	if pgCode(err) == "53300" { // too_many_connections
		return true
	}
	return containsAny(err, "no pg_hba.conf entry")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return containsAny(err, "i/o timeout", "timeout expired", "canceling statement due to statement timeout")
}

func isUnreachable(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return containsAny(err, "connection refused", "no such host", "network is unreachable",
		"host is unreachable", "connection reset by peer", "broken pipe", "eof")
}
