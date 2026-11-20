// Package domain (ProjectLink) is the query text of a project's links
// (basecamp-16, migration 544): business/ProjectLink owns their rules, and
// models/postgres/ProjectLink runs the text through models/pgexec.
package domain

import softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"

// Columns is what a read of a link returns, in its scan's order.
const Columns = `id, project_uuid, url, title, description, site_name, icon_object, image_object, position, created_at`

// Counts is how many live links a project has ($1) and whether one of them
// has one of the addresses in $2 (a text array; NULL or empty asks after
// none), in one row.
var Counts = `SELECT COUNT(*), COALESCE(bool_or(url = ANY($2)), false) FROM project_links WHERE project_uuid = $1 AND ` + softDelete.Live("")

// Insert adds links to a project ($1), last in its order in the order given,
// in one statement: one link per element of the text arrays $2 (addresses),
// $3 (titles), $4 (descriptions), $5 (site names), $6 and $7 (the stored
// icon's and picture's objects, an empty text for none), all by $8 (NULL: no one
// person). It returns the links made, Columns each.
var Insert = `INSERT INTO project_links (project_uuid, url, title, description, site_name, icon_object, image_object, created_by, position)
	SELECT $1::uuid, l.url, l.title, l.description, l.site_name, NULLIF(l.icon, '')::uuid, NULLIF(l.image, '')::uuid, $8::uuid,
		COALESCE((SELECT MAX(position) FROM project_links WHERE project_uuid = $1::uuid AND ` + softDelete.Live("") + `), 0) + l.n
	FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
		WITH ORDINALITY AS l(url, title, description, site_name, icon, image, n)
	RETURNING ` + Columns
