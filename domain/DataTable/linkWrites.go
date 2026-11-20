package tableDomain

// How a write makes links, in its transaction: models/postgres/DataTable
// changeLinksMade runs them through pgexec, one of each per field however
// many rows the write links.

// MakeLinksSQL makes the links of field $1 from $2[i] to $3[i], each at NOW()
// plus $4[i] microseconds, $4[i] being the link's place among those its row
// was given (so a row's links keep their order, rows made together or not),
// and gives back those it made: a link already there is left as it is.
const MakeLinksSQL = `INSERT INTO data_table_links (field_id, from_row, to_row, created_at)
	SELECT $1, p.f, p.t, NOW() + p.i * INTERVAL '1 microsecond' FROM unnest($2::uuid[], $3::uuid[], $4::bigint[]) AS p(f, t, i)
	ON CONFLICT DO NOTHING RETURNING from_row, to_row, created_at`

// OverLinkedSQL counts the rows among $2 that link to more than $3 rows
// through field $1.
const OverLinkedSQL = `SELECT COUNT(*) FROM (SELECT from_row FROM data_table_links
	WHERE field_id = $1 AND from_row = ANY($2) GROUP BY from_row HAVING COUNT(*) > $3) x`
