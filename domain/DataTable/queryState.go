package tableDomain

// What a rows query reads of its table before its window, in one round
// trip: its live fields, its cell index's state, and whether a live row
// waits for its place key. models/postgres/DataTable runs them through
// pgexec (ReadQueryState); the first two also run alone (LiveFields,
// CellIndexState).

// LiveFieldsSQL reads the live fields of the table $1, in its order.
const LiveFieldsSQL = `SELECT id, table_id, name, type, config, position, description, required, default_value, is_primary, created_at, updated_at, deleted_at
	FROM data_table_fields WHERE table_id = $1 AND deleted_at IS NULL ORDER BY position ASC, created_at ASC`

// CellIndexStateSQL reads when the table $1's cell index was made whole
// (NULL while it isn't) and the version that made it so.
const CellIndexStateSQL = `SELECT cell_index_at, cell_index_version FROM data_tables WHERE id = $1`

// HasUnkeyedRowsSQL is whether a live row of the table $1 has no place key
// yet (a row a server from before the keys made; StartLinkUpkeep keys them):
// one lookup of the partial index on data_table_rows (table_id) WHERE
// position_key IS NULL, which is empty almost always.
const HasUnkeyedRowsSQL = `SELECT EXISTS (SELECT 1 FROM data_table_rows
		WHERE table_id = $1 AND position_key IS NULL AND deleted_at IS NULL)`
