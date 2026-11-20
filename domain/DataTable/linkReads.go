package tableDomain

// The reads a rows page makes of what its rows link to, one statement for
// the whole page whatever its relations: the linked tables and their fields,
// how many links each row holds, which rows those are, and the linked rows'
// cells. models/postgres/DataTable runs them through pgexec.

// OpenTablesSQL reads the live tables among $1.
const OpenTablesSQL = `SELECT id, name, description, icon, visibility, created_by, created_at, updated_at, deleted_at, general_level
	FROM data_tables WHERE id = ANY($1) AND deleted_at IS NULL`

// FieldsOfTablesSQL reads the live fields of the tables $1, each table's in
// its own order.
const FieldsOfTablesSQL = `SELECT id, table_id, name, type, config, position, description, required, default_value, is_primary, created_at, updated_at, deleted_at
	FROM data_table_fields WHERE table_id = ANY($1) AND deleted_at IS NULL
	ORDER BY table_id, position ASC, created_at ASC`

// CountLinksSQL counts, up to $7, the links each asked row holds: read $1[i]
// asks field $2[i] for row $3[i] at the link's from end, read $4[i] asks
// field $5[i] for row $6[i] at its to end. Each side keeps the index it had
// when a field was counted alone.
const CountLinksSQL = `SELECT r.i, r.id, (SELECT COUNT(*) FROM (SELECT 1 FROM data_table_links l
		WHERE l.field_id = r.f AND l.from_row = r.id LIMIT $7) x)
	FROM unnest($1::int[], $2::uuid[], $3::uuid[]) AS r(i, f, id)
	UNION ALL
	SELECT r.i, r.id, (SELECT COUNT(*) FROM (SELECT 1 FROM data_table_links l
		WHERE l.field_id = r.f AND l.to_row = r.id LIMIT $7) x)
	FROM unnest($4::int[], $5::uuid[], $6::uuid[]) AS r(i, f, id)`

// LinksOfSQL reads the first $4[i] (from end) or $8[i] (to end) rows each
// asked row links to, as CountLinksSQL pairs its arguments.
const LinksOfSQL = `SELECT r.i, r.id, x.other, x.created_at
	FROM unnest($1::int[], $2::uuid[], $3::uuid[], $4::int[]) AS r(i, f, id, k)
	CROSS JOIN LATERAL (SELECT l.to_row AS other, l.created_at FROM data_table_links l
		WHERE l.field_id = r.f AND l.from_row = r.id
		ORDER BY l.created_at, l.to_row LIMIT r.k) x
	UNION ALL
	SELECT r.i, r.id, x.other, x.created_at
	FROM unnest($5::int[], $6::uuid[], $7::uuid[], $8::int[]) AS r(i, f, id, k)
	CROSS JOIN LATERAL (SELECT l.from_row AS other, l.created_at FROM data_table_links l
		WHERE l.field_id = r.f AND l.to_row = r.id
		ORDER BY l.created_at, l.from_row LIMIT r.k) x`

// CellsOfSQL reads the live rows $1 of the tables $2 (paired), each with
// only the cells its table asks for: key $3[j] of table $4[j].
const CellsOfSQL = `SELECT r.table_id, r.id, r.row_no, r.created_by, r.created_at, r.updated_at, r.updated_by,
		COALESCE((SELECT jsonb_object_agg(e.key, e.value) FROM jsonb_each(r.values) e
			WHERE e.key IN (SELECT k.key FROM unnest($3::text[], $4::uuid[]) AS k(key, t) WHERE k.t = r.table_id)), '{}'::jsonb)
	FROM unnest($1::uuid[], $2::uuid[]) AS w(id, t)
	JOIN data_table_rows r ON r.id = w.id AND r.table_id = w.t
	WHERE r.deleted_at IS NULL`
