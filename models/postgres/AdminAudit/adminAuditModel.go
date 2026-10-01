// Package models (AdminAudit) is the data-access layer for the admin config
// audit log. Append-only by design: there is no update or delete path.
//
// Tamper-evidence (migration 106): each entry stores entry_hash =
// SHA-256(prev_hash + canonical content), forming a hash chain. Any later
// insert/edit/delete of a historical row breaks Verify. Inserts are serialized
// under a Postgres advisory lock so the chain stays linear even though callers
// write asynchronously. Pre-feature rows have NULL hashes and are skipped by
// verification (the chain starts at the first hashed row).
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Categories for grouping/filtering audit entries in the admin UI.
const (
	CategorySettings    = "settings"
	CategoryIntegration = "integration"
	CategoryAuth        = "auth"
	CategoryApp         = "app"
	CategorySecurity    = "security"
	// CategoryAgent is work an AGENT did on a person's behalf, including calls
	// arriving over MCP from outside the workspace. Separate because "which of
	// these was a human and which was an agent acting for one" is the first
	// question an auditor asks, and answering it with a query rather than a
	// judgement call is the difference between an audit trail and a log file.
	CategoryAgent = "agent"
)

// AllCategories is the canonical, ordered list of audit categories.
//
// EXISTS TO STOP THE UI DRIFTING FROM THE DATA. The admin audit log offers a
// filter button per category, and that list used to be hardcoded in the frontend.
// CategoryAgent was added later — declared off on its own in the business package
// rather than here — so the UI never learned about it, and every agent and MCP
// entry, including every REFUSAL, landed in the log unfilterable. For a product
// whose case rests on being auditable, evidence you cannot select is close to
// evidence you do not have.
//
// So the server now tells the client what the categories are, and the client
// renders whatever it is given. A category added here appears in the UI without
// anyone remembering to go and add it.
func AllCategories() []string {
	return []string{
		CategorySettings,
		CategoryIntegration,
		CategoryAuth,
		CategoryApp,
		CategorySecurity,
		CategoryAgent,
	}
}

// auditChainLockKey is the advisory-lock key that serializes audit inserts so
// the hash chain is built in a single linear order under concurrency.
const auditChainLockKey int64 = 472074

// AuditEntry mirrors a row of admin_audit_log.
type AuditEntry struct {
	Id         uuid.UUID  `json:"id"`
	Seq        int64      `json:"seq"`
	ActorID    *uuid.UUID `json:"actor_id,omitempty"`
	ActorEmail string     `json:"actor_email,omitempty"`
	// ActorKind is who acted: a person, an agent, or the system.
	//
	// ActorID names WHO IS ACCOUNTABLE, which for an agent is the human whose
	// credential it used. That is the right answer to "who answers for this" and
	// the wrong answer to "did a person do this", and until now the row only
	// carried the first. Empty on every entry written before this column
	// existed, which is why the hash appends it only when set.
	ActorKind string    `json:"actor_kind,omitempty"`
	Action    string    `json:"action"`
	Category  string    `json:"category"`
	Summary   string    `json:"summary"`
	Metadata  *string   `json:"metadata,omitempty"`
	IPAddress string    `json:"ip_address,omitempty"`
	UserAgent string    `json:"user_agent,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	EntryHash string    `json:"entry_hash,omitempty"`
	PrevHash  string    `json:"prev_hash,omitempty"`

	// RedactedAt marks a row whose content was cleared by retention. The row and
	// its hashes stay so the chain is unbroken; what is gone is the ability to
	// recompute this row's hash from its content.
	RedactedAt *time.Time `json:"redacted_at,omitempty"`
}

// computeAuditHash derives an entry's chain hash from the previous entry's hash
// and this entry's canonical content. Deterministic: created_at is truncated to
// microseconds (Postgres timestamp precision) so a recomputation after a DB
// round-trip yields the identical string.
// canonicalJSON renders a JSON document in one stable form, so a value that has
// been through Postgres hashes the same as the value that went in.
//
// THE BUG THIS FIXES. metadata is a jsonb column, and jsonb does not store the
// bytes it was given: it parses them, reorders object keys by length and then
// bytewise, and re-renders with a space after every colon and comma. Insert hashed
// the Go-marshalled string, which is key-sorted alphabetically and compact;
// Verify recomputed from whatever jsonb handed back. Those never matched. A single
// key was enough -- Go writes {"mode":"backend"} and jsonb returns
// {"mode": "backend"} -- so EVERY audit entry carrying metadata reported the chain
// broken. On the installation this was found on, that was all 33 rows, from the
// first one, and the product's headline compliance claim was reporting tampering
// on a log nobody had touched.
//
// Canonicalising inside computeAuditHash fixes both halves at one point, and
// leaves every STORED hash valid: Insert was already hashing Go's canonical
// rendering, so canonicalising an already-canonical string is a no-op there,
// while Verify now recomputes the same bytes Insert did. No migration, no rehash,
// and an install that has been reporting a broken chain starts reporting the truth
// about it.
//
// UseNumber so a large integer is not routed through float64 and re-rendered with
// a different number of digits. Anything that is not JSON we can parse is hashed
// verbatim, which is the old behaviour and the safe one: it can only fail closed.
func canonicalJSON(raw string) string {
	if raw == "" {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(out)
}

func computeAuditHash(prevHash string, e *AuditEntry) string {
	meta := ""
	if e.Metadata != nil {
		// Canonical, not verbatim: jsonb re-renders what it was given. See
		// canonicalJSON.
		meta = canonicalJSON(*e.Metadata)
	}
	actorID := ""
	if e.ActorID != nil {
		actorID = e.ActorID.String()
	}
	var b strings.Builder
	b.WriteString(prevHash)
	b.WriteByte('\n')
	b.WriteString(e.Id.String())
	b.WriteByte('\n')
	b.WriteString(e.CreatedAt.UTC().Format(time.RFC3339Nano))
	b.WriteByte('\n')
	b.WriteString(actorID)
	b.WriteByte('\n')
	b.WriteString(e.ActorEmail)
	b.WriteByte('\n')
	b.WriteString(e.Action)
	b.WriteByte('\n')
	b.WriteString(e.Category)
	b.WriteByte('\n')
	b.WriteString(e.Summary)
	b.WriteByte('\n')
	b.WriteString(meta)
	// APPENDED ONLY WHEN SET, and that is the whole design of this addition.
	//
	// Every stored entry hashes the previous one, so changing what goes into the
	// hash would invalidate the entire chain rather than just the next link. An
	// entry written before this column existed has no kind, contributes nothing
	// here, and hashes exactly as it did. A new entry carries its kind inside
	// the hash, so editing the row to hide that an agent acted changes the
	// recomputed hash and is caught by the same verification as any other edit.
	if e.ActorKind != "" {
		b.WriteByte('\n')
		b.WriteString(e.ActorKind)
	}
	return helpers.SHA256Hex(b.String())
}

// Insert appends an audit entry and links it into the hash chain. Best-effort:
// callers log but do not fail the originating action if the audit write fails.
// The advisory lock serializes concurrent inserts so the chain is linear.
func Insert(ctx context.Context, e *AuditEntry) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.Insert begin err: %+v", err)
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err = tx.ExecContext(dbctx, `SELECT pg_advisory_xact_lock($1)`, auditChainLockKey); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.Insert lock err: %+v", err)
		return err
	}

	var prev sql.NullString
	if err = tx.QueryRowContext(dbctx,
		`SELECT entry_hash FROM admin_audit_log WHERE entry_hash IS NOT NULL ORDER BY seq DESC LIMIT 1`).Scan(&prev); err != nil && err != sql.ErrNoRows {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.Insert prev err: %+v", err)
		return err
	}

	if e.Id == uuid.Nil {
		e.Id = uuid.New()
	}
	e.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	e.PrevHash = prev.String
	e.EntryHash = computeAuditHash(e.PrevHash, e)

	const q = `
		INSERT INTO admin_audit_log
			(id, actor_id, actor_email, action, category, summary, metadata,
			 ip_address, user_agent, created_at, entry_hash, prev_hash, actor_kind)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	if _, err = tx.ExecContext(dbctx, q,
		e.Id, e.ActorID, nullStr(e.ActorEmail), e.Action, e.Category, e.Summary,
		e.Metadata, nullStr(e.IPAddress), nullStr(e.UserAgent), e.CreatedAt,
		e.EntryHash, nullStr(e.PrevHash), nullStr(e.ActorKind)); err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.Insert err: %+v", err)
		return err
	}
	return tx.Commit()
}

const auditCols = `id, seq, actor_id, actor_email, action, category, summary, metadata, ip_address, user_agent, created_at, entry_hash, prev_hash, redacted_at, actor_kind`

func scanEntry(rows *sql.Rows) (*AuditEntry, error) {
	var e AuditEntry
	var actorID uuid.NullUUID
	var actorEmail, metadata, ip, ua, entryHash, prevHash, actorKind sql.NullString
	var redactedAt sql.NullTime
	if err := rows.Scan(
		&e.Id, &e.Seq, &actorID, &actorEmail, &e.Action, &e.Category, &e.Summary,
		&metadata, &ip, &ua, &e.CreatedAt, &entryHash, &prevHash, &redactedAt, &actorKind,
	); err != nil {
		return nil, err
	}
	if redactedAt.Valid {
		t := redactedAt.Time
		e.RedactedAt = &t
	}
	if actorID.Valid {
		e.ActorID = &actorID.UUID
	}
	e.ActorEmail = actorEmail.String
	if metadata.Valid {
		e.Metadata = &metadata.String
	}
	e.IPAddress = ip.String
	e.UserAgent = ua.String
	e.EntryHash = entryHash.String
	e.PrevHash = prevHash.String
	e.ActorKind = actorKind.String
	return &e, nil
}

// List returns audit entries newest-first (by chain order), optionally filtered
// by category, with simple limit/offset pagination.
// ListFilter narrows a listing. Every field is optional and they compose.
//
// A struct rather than a growing positional signature, because the third
// filter is where "category, limit, offset" turns into a call nobody can read
// at the call site. Initiators filters on the metadata key the audit business
// layer stamps on agent rows; see business/AdminAudit/initiator.go.
type ListFilter struct {
	Category string
	// Initiators keeps rows whose metadata initiator is one of these. Empty
	// means no filter. A row with no initiator at all never matches, which is
	// deliberate: it is a person's own action or a row written before the key
	// existed, and neither is an answer to "what ran while nobody was there".
	Initiators []string
}

// whereClause builds the WHERE for a filter, numbering placeholders from 1.
// Returned with its arguments so the caller cannot mis-order them.
func (f ListFilter) whereClause() (string, []interface{}) {
	clauses := []string{}
	args := []interface{}{}
	if f.Category != "" {
		args = append(args, f.Category)
		clauses = append(clauses, fmt.Sprintf("category = $%d", len(args)))
	}
	if len(f.Initiators) > 0 {
		args = append(args, pq.Array(f.Initiators))
		clauses = append(clauses, fmt.Sprintf("(metadata ->> 'initiator') = ANY($%d)", len(args)))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// List returns entries newest first, narrowed by the filter.
func List(ctx context.Context, category string, limit, offset int) ([]*AuditEntry, error) {
	return ListFiltered(ctx, ListFilter{Category: category}, limit, offset)
}

// ListFiltered is List with the full filter.
func ListFiltered(ctx context.Context, f ListFilter, limit, offset int) ([]*AuditEntry, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	where, args := f.whereClause()
	args = append(args, limit, offset)
	query := fmt.Sprintf(`SELECT `+auditCols+` FROM admin_audit_log%s ORDER BY seq DESC LIMIT $%d OFFSET $%d`,
		where, len(args)-1, len(args))
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.List err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AuditEntry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListByActionPrefixes returns audit entries whose action starts with ANY of
// the given prefixes (e.g. "ai." or "api."), newest-first. Generic helper used
// by the unified AI activity timeline so it can pull only AI-attributable
// entries without scanning the whole log. Empty prefixes → no rows.
func ListByActionPrefixes(ctx context.Context, prefixes []string, actorID *uuid.UUID, limit int) ([]*AuditEntry, error) {
	if len(prefixes) == 0 {
		return nil, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 200 {
		limit = 50
	}

	clauses := make([]string, 0, len(prefixes))
	args := make([]interface{}, 0, len(prefixes)+2)
	for i, p := range prefixes {
		clauses = append(clauses, "action LIKE $"+itoa(i+1))
		args = append(args, p+"%")
	}

	// actorID scopes the read to one person's own decisions.
	//
	// WHY IT EXISTS. The workspace-wide log is admin-only and should stay that
	// way: it carries other people's actions and every configuration change. But
	// "an agent acting for me was refused" is a fact about ME, and a member who
	// cannot see it has to take the product's central guarantee on trust — which
	// is exactly the trust this product exists to replace.
	//
	// nil means every actor, which is the admin's view. One query builder rather
	// than two functions, because the difference is a WHERE clause and two
	// near-identical readers would eventually disagree about the column list.
	actorClause := ""
	if actorID != nil {
		args = append(args, *actorID)
		actorClause = " AND actor_id = $" + itoa(len(args))
	}

	args = append(args, limit)
	q := `SELECT ` + auditCols + ` FROM admin_audit_log WHERE (` +
		strings.Join(clauses, " OR ") + `)` + actorClause +
		` ORDER BY seq DESC LIMIT $` + itoa(len(args))

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.ListByActionPrefixes err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*AuditEntry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// itoa is a tiny strconv.Itoa wrapper kept local so the query builder reads
// cleanly; placeholders are integer-only so this can never inject.
func itoa(n int) string { return strconv.Itoa(n) }

// ListForExport returns up to a large bound of entries (oldest-first, the chain
// order) for an admin export, optionally filtered by category and date range.
func ListForExport(ctx context.Context, category string, from, to *time.Time) ([]*AuditEntry, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const maxExport = 10000
	var rows *sql.Rows
	var err error

	// Build the WHERE clause dynamically. Only category and date range are
	// supported filters, and date values come from the caller's parsed input.
	clauses := []string{}
	args := []interface{}{}
	idx := 0
	next := func() string {
		idx++
		return "$" + strconv.Itoa(idx)
	}

	if category != "" {
		clauses = append(clauses, "category = "+next())
		args = append(args, category)
	}
	if from != nil {
		clauses = append(clauses, "created_at >= "+next())
		args = append(args, *from)
	}
	if to != nil {
		clauses = append(clauses, "created_at <= "+next())
		args = append(args, *to)
	}

	where := ""
	if len(clauses) > 0 {
		where = "WHERE " + strings.Join(clauses, " AND ")
	}

	args = append(args, maxExport)
	q := `SELECT ` + auditCols + ` FROM admin_audit_log ` + where + ` ORDER BY seq ASC LIMIT $` + strconv.Itoa(idx+1)

	rows, err = postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.ListForExport err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VerifyResult reports the outcome of a chain verification.
type VerifyResult struct {
	OK          bool       `json:"ok"`
	Checked     int        `json:"checked"`
	FirstBadSeq *int64     `json:"first_bad_seq,omitempty"`
	FirstBadID  *uuid.UUID `json:"first_bad_id,omitempty"`
	Message     string     `json:"message"`

	// Redacted is how many rows had their content cleared by retention and so
	// could not be recomputed from it. Reported separately from Checked rather
	// than folded into it, because "I verified this" and "I took this row's
	// word for it" are different statements and an auditor is entitled to both
	// numbers.
	Redacted int `json:"redacted"`

	// Partial says this run checked a WINDOW, not the whole log, and FromSeq
	// says where the window started.
	//
	// Disclosed rather than inferred, because the two are different claims and
	// only one of them is "the log has not been altered". A window verifies the
	// links inside it and SEEDS from the first row's stored prev_hash, so it
	// cannot detect tampering before that point -- it takes that one value on
	// trust. A caller that says "verified" without saying which of the two it
	// did is overstating a compliance result, which is the one kind of bug this
	// subsystem must not have.
	Partial bool  `json:"partial"`
	FromSeq int64 `json:"from_seq,omitempty"`
}

// Verify recomputes the hash chain over all hashed rows (oldest-first) and
// reports the first divergence, if any. A tampered, inserted, or deleted
// historical row breaks the recomputed chain.
func Verify(ctx context.Context) (*VerifyResult, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT `+auditCols+` FROM admin_audit_log WHERE entry_hash IS NOT NULL ORDER BY seq ASC`)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.Verify err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var entries []*AuditEntry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Seeded with the empty string: a full verification starts at the genesis
	// row, whose prev_hash is empty, and therefore trusts nothing.
	return verifyChain(entries, "", false, 0), nil
}

// VerifyRecent recomputes the chain over the last limit hashed rows.
//
// WHY A WINDOW EXISTS AT ALL. Verify walks every hashed row, which is the right
// answer for an auditor and the wrong one for anything on a request path: the log
// only grows, so a call that is instant on a fresh install becomes a slow query
// and then a proxy timeout on a workspace that has been running for a year. The
// governance drill verifies the chain as its last step and is clicked from a
// browser, so it needs an answer bounded by something other than the customer's
// age.
//
// WHAT IT COSTS, AND THIS IS THE POINT. A window seeds from the stored prev_hash
// of its earliest row instead of from genesis, so it proves the links inside the
// window and takes that one value on trust. It cannot see tampering before the
// window. The result says so -- Partial and FromSeq -- and every caller is
// expected to pass that through rather than render it as "the log is intact".
func VerifyRecent(ctx context.Context, limit int) (*VerifyResult, error) {
	if limit <= 0 {
		limit = 500
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Newest-first with a LIMIT so the database can stop early, then reversed in
	// memory: ordering ascending and offsetting would read the whole table to
	// find the tail, which is the cost this function exists to avoid.
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx,
		`SELECT `+auditCols+` FROM admin_audit_log WHERE entry_hash IS NOT NULL ORDER BY seq DESC LIMIT $1`, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.VerifyRecent err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var reversed []*AuditEntry
	for rows.Next() {
		e, scanErr := scanEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		reversed = append(reversed, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	entries := make([]*AuditEntry, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		entries = append(entries, reversed[i])
	}
	if len(entries) == 0 {
		return &VerifyResult{OK: true, Message: "no hashed entries to verify"}, nil
	}

	// A window that happens to reach the start of the log is a FULL verification
	// and says so, rather than warning about a limit it did not hit.
	seed := entries[0].PrevHash
	partial := seed != ""
	from := int64(0)
	if partial {
		from = entries[0].Seq
	}
	return verifyChain(entries, seed, partial, from), nil
}

// verifyChain recomputes a run of entries against a starting hash. Shared by the
// full walk and the windowed one so the two can never drift into disagreeing
// about what a valid chain is.
func verifyChain(entries []*AuditEntry, seed string, partial bool, fromSeq int64) *VerifyResult {
	prev := seed
	checked := 0
	redacted := 0
	for _, e := range entries {
		// A redacted row cannot be recomputed: its content is gone by design.
		// Its stored hash still links the chain, so continuity is preserved and
		// the row is counted as redacted rather than as verified. Tampering with
		// a redacted row's HASH still breaks the next row, because the next row
		// hashed this one's hash.
		if e.RedactedAt != nil {
			prev = e.EntryHash
			redacted++
			continue
		}
		want := computeAuditHash(prev, e)
		if want != e.EntryHash {
			seq := e.Seq
			id := e.Id
			return &VerifyResult{
				OK: false, Checked: checked, Redacted: redacted, FirstBadSeq: &seq, FirstBadID: &id,
				Partial: partial, FromSeq: fromSeq,
				Message: "audit chain broken: an entry was modified, inserted, or deleted",
			}
		}
		prev = e.EntryHash
		checked++
	}
	msg := "audit chain intact"
	if partial {
		msg = fmt.Sprintf("the last %d entries verify, from entry %d onwards", checked, fromSeq)
	}
	if redacted > 0 {
		msg += fmt.Sprintf("; %d redacted row(s) verified by link only, their content having been cleared by retention", redacted)
	}
	return &VerifyResult{OK: true, Checked: checked, Redacted: redacted, Partial: partial, FromSeq: fromSeq, Message: msg}
}

func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// RedactOlderThan clears the content of audit entries older than cutoff and
// marks them redacted, returning how many were affected.
//
// Clears content, keeps the row. Deleting would break the hash chain for every
// entry after it, which would trade the log's tamper-evidence for its tidiness.
// The columns cleared are the ones that carry personal data or free text: who
// did it, what it said, and from where. Action, category, timestamp and the
// hashes remain, so a redacted row still shows that SOMETHING of a given kind
// happened at a given time, which is usually what a retention policy means to
// keep.
//
// Idempotent: already-redacted rows are skipped, so a sweep that runs twice
// does nothing the second time.
func RedactOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE admin_audit_log
	              SET actor_email = NULL,
	                  summary     = '',
	                  metadata    = NULL,
	                  ip_address  = NULL,
	                  user_agent  = NULL,
	                  redacted_at = NOW()
	            WHERE created_at < $1 AND redacted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, cutoff)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdminAudit.RedactOlderThan err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
