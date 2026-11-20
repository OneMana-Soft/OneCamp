package tableDomain

// Query text models/postgres/DataTable runs through models/pgexec.

// SoftDeleteTables puts the live ones among tables ($1, their ids) in the
// trash, by $2 (NULL: no one person), in one statement; it changes one row
// per table it deleted.
const SoftDeleteTables = `UPDATE data_tables SET deleted_at=NOW(), updated_at=NOW(), deleted_by=$2 WHERE id = ANY($1) AND deleted_at IS NULL`

// PagesOfTables is every page the rows of tables ($1, their ids) host,
// deleted rows' too, in one read.
const PagesOfTables = `SELECT doc_uuid FROM data_table_rows WHERE table_id = ANY($1) AND doc_uuid IS NOT NULL`
