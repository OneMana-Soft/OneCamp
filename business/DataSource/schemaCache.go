package business

// schemaCache is a tiny, thread-safe, per-source TTL cache of introspected
// schemas. Without it, every agent query would re-read the external database's
// full information_schema (a real latency + load cost against a customer's
// warehouse, and redundant when an agent runs several queries in a row). The
// cache is invalidated whenever a source's config changes (edit/enable/delete),
// so a stale entry can never outlive a connection change. It is purely an
// optimization: the cached schema is still what the customer's DB returned, so
// identifier validation stays authoritative and safe.

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// schemaTTL is deliberately short: long enough to serve a burst of queries from
// one agent turn, short enough that a newly-added column shows up quickly.
const schemaTTL = 60 * time.Second

type schemaEntry struct {
	tables  []TableSchema
	expires time.Time
}

var (
	schemaMu    sync.RWMutex
	schemaCache = map[uuid.UUID]schemaEntry{}
)

// getCachedSchema returns a live cached schema for a source, or (nil,false).
func getCachedSchema(id uuid.UUID) ([]TableSchema, bool) {
	schemaMu.RLock()
	e, ok := schemaCache[id]
	schemaMu.RUnlock()
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.tables, true
}

// setCachedSchema stores a source's schema with the TTL.
func setCachedSchema(id uuid.UUID, tables []TableSchema) {
	schemaMu.Lock()
	schemaCache[id] = schemaEntry{tables: tables, expires: time.Now().Add(schemaTTL)}
	schemaMu.Unlock()
}

// invalidateSchema drops a source's cached schema (called on any config change),
// so the next read re-introspects against the current connection.
func invalidateSchema(id uuid.UUID) {
	schemaMu.Lock()
	delete(schemaCache, id)
	schemaMu.Unlock()
}
