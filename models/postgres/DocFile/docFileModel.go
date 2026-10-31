package models

// Postgres access for the doc tree's files (docs F1, Docs & Files;
// migrations/476_doc_file_versions.up.sql): each version of a file node is its
// own attachments row, at place doc with src_value the file node, so
// FileAccess's doc rule answers every version and nothing new decides access;
// and a doc_file_versions row names it with what Postgres keeps of no
// attachment: its name, size and type, an optional note, who added it and
// when. The node's file is its highest n. business/Doc owns the rules; this
// only reads and writes rows, through models/pgexec.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Version is one version of a file.
type Version struct {
	DocUUID      uuid.UUID
	N            int
	AttachmentID uuid.UUID
	Name         string
	SizeBytes    int64
	MIME         string
	Note         string
	CreatedBy    uuid.UUID
	CreatedAt    time.Time
}

// Current is a file's newest version, how many it has, and the stored key
// its attachments row names: what a tree or list row and a search entry show
// of it.
type Current struct {
	Version
	Versions int
	ObjKey   string
}

// NewVersion is a version to record: the stored upload's key (the
// attachments row's obj_key) beside the version's facts.
type NewVersion struct {
	DocUUID      uuid.UUID
	AttachmentID uuid.UUID
	ObjKey       string
	Name         string
	SizeBytes    int64
	MIME         string
	Note         string
	CreatedBy    uuid.UUID
}

// errNotRecorded is a version's statement that wrote nothing.
var errNotRecorded = errors.New("doc file: the version wasn't recorded")

// recordVersion writes a version's attachments row and its version row, the
// next n of the file's, in one statement: both or neither.
const recordVersion = `
	WITH att AS (
		INSERT INTO attachments (id, obj_key, src_key, src_value, created_by)
		SELECT $2::uuid, $3, $4, $1::uuid::text, $8::uuid
		RETURNING id
	)
	INSERT INTO doc_file_versions (doc_uuid, n, attachment_id, name, size_bytes, mime, note, created_by)
	SELECT $1::uuid, COALESCE((SELECT max(n) FROM doc_file_versions WHERE doc_uuid = $1::uuid), 0) + 1, att.id, $5, $6, $7, NULLIF($9, ''), $8::uuid
	FROM att
	RETURNING n, created_at`

// RecordVersion records a new version of the file v names, as its next n,
// and answers it. Two at once for one file are taken in turn (a lock on the
// file for the transaction), so each gets its own n.
func RecordVersion(ctx context.Context, v NewVersion) (Version, error) {
	out := Version{DocUUID: v.DocUUID, AttachmentID: v.AttachmentID, Name: v.Name, SizeBytes: v.SizeBytes,
		MIME: v.MIME, Note: v.Note, CreatedBy: v.CreatedBy}
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "doc_file_versions:"+v.DocUUID.String()); err != nil {
			return err
		}
		row, found, err := pgexec.QueryOne(ctx, recordVersion, func(r pgexec.Row) (Version, error) {
			got := out
			err := r.Scan(&got.N, &got.CreatedAt)
			return got, err
		}, v.DocUUID.String(), v.AttachmentID.String(), v.ObjKey, postgresStruct.ATTACHMENT_SRC_DOC, v.Name, v.SizeBytes, v.MIME, v.CreatedBy.String(), v.Note)
		if err == nil && !found {
			err = errNotRecorded
		}
		out = row
		return err
	})
	return out, err
}

// versionColumns are a version's columns, as scanVersion reads them.
const versionColumns = `v.doc_uuid, v.n, v.attachment_id, v.name, v.size_bytes, v.mime, v.note, v.created_by, v.created_at`

// scanVersion reads a row of versionColumns.
func scanVersion(r pgexec.Row) (Version, error) {
	var v Version
	var note sql.NullString
	err := r.Scan(&v.DocUUID, &v.N, &v.AttachmentID, &v.Name, &v.SizeBytes, &v.MIME, &note, &v.CreatedBy, &v.CreatedAt)
	v.Note = note.String
	return v, err
}

// Versions is every version of a file, newest first.
func Versions(ctx context.Context, docUUID uuid.UUID) ([]Version, error) {
	return pgexec.Query(ctx, `
		SELECT `+versionColumns+`
		FROM doc_file_versions v
		WHERE v.doc_uuid = $1::uuid
		ORDER BY v.n DESC`, scanVersion, docUUID.String())
}

// uuidTexts is ids as an = ANY parameter takes them.
func uuidTexts(ids []uuid.UUID) any {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return pq.Array(out)
}

// currentColumns are a file's newest version and its count, as scanCurrent
// reads them.
const currentColumns = `v.doc_uuid, v.n, v.attachment_id, v.name, v.size_bytes, v.mime, v.note, v.created_by, v.created_at, count(*) OVER (PARTITION BY v.doc_uuid), a.obj_key`

// scanCurrent reads a row of currentColumns.
func scanCurrent(r pgexec.Row) (Current, error) {
	var c Current
	var note sql.NullString
	err := r.Scan(&c.DocUUID, &c.N, &c.AttachmentID, &c.Name, &c.SizeBytes, &c.MIME, &note, &c.CreatedBy, &c.CreatedAt, &c.Versions, &c.ObjKey)
	c.Note = note.String
	return c, err
}

// CurrentOf is each named file's newest version and its count, by file, in
// one read: the files of a tree's level or a list's page. A doc with no
// versions (a page, a folder) is left out.
func CurrentOf(ctx context.Context, docUUIDs []uuid.UUID) (map[uuid.UUID]Current, error) {
	out := map[uuid.UUID]Current{}
	if len(docUUIDs) == 0 {
		return out, nil
	}
	rows, err := pgexec.Query(ctx, `
		SELECT DISTINCT ON (v.doc_uuid) `+currentColumns+`
		FROM doc_file_versions v JOIN attachments a ON a.id = v.attachment_id
		WHERE v.doc_uuid = ANY($1::uuid[])
		ORDER BY v.doc_uuid, v.n DESC`, scanCurrent, uuidTexts(docUUIDs))
	for _, c := range rows {
		out[c.DocUUID] = c
	}
	return out, err
}

// ForgetFiles takes the versions of files gone for good (a purge) and
// archives each one's attachments row at at, in one statement: the
// attachments purge then removes their bytes once its days have passed
// (business/Archive purge.go), unless another live row names the same object
// (a duplicate's copy). A doc with no versions is left as it was. It answers
// how many rows it archived.
func ForgetFiles(ctx context.Context, docUUIDs []uuid.UUID, at time.Time) (int64, error) {
	if len(docUUIDs) == 0 {
		return 0, nil
	}
	return pgexec.Exec(ctx, `
		WITH gone AS (
			DELETE FROM doc_file_versions WHERE doc_uuid = ANY($1::uuid[]) RETURNING attachment_id
		)
		UPDATE attachments SET deleted_at = $2
		WHERE id IN (SELECT attachment_id FROM gone) AND deleted_at IS NULL`, uuidTexts(docUUIDs), at)
}

// DropUnseen removes files' versions and their attachments rows outright:
// for files whose nodes were never made (the other half of their write
// failed), so nobody has seen them. It answers how many attachments rows went.
func DropUnseen(ctx context.Context, docUUIDs []uuid.UUID) (int64, error) {
	if len(docUUIDs) == 0 {
		return 0, nil
	}
	return pgexec.Exec(ctx, `
		WITH gone AS (
			DELETE FROM doc_file_versions WHERE doc_uuid = ANY($1::uuid[]) RETURNING attachment_id
		)
		DELETE FROM attachments WHERE id IN (SELECT attachment_id FROM gone)`, uuidTexts(docUUIDs))
}

// Copy is one file a duplicate copies: from the source file to the copy's
// node, with the attachment id its copy's row takes.
type Copy struct {
	From, To, AttachmentID uuid.UUID
}

// CopyCurrent gives each copy its source's newest version as its version 1:
// a new attachments row on the same stored object (which a purge then leaves
// while either row lives), at place doc with src_value the copy, by by; in
// one statement for every copy. It answers how many it copied.
func CopyCurrent(ctx context.Context, copies []Copy, by uuid.UUID) (int64, error) {
	if len(copies) == 0 {
		return 0, nil
	}
	from := make([]uuid.UUID, len(copies))
	to := make([]uuid.UUID, len(copies))
	ids := make([]uuid.UUID, len(copies))
	for i, c := range copies {
		from[i], to[i], ids[i] = c.From, c.To, c.AttachmentID
	}
	return pgexec.Exec(ctx, `
		WITH pairs AS (
			SELECT * FROM unnest($1::uuid[], $2::uuid[], $3::uuid[]) AS p(src, dst, att)
		), src AS (
			SELECT DISTINCT ON (v.doc_uuid) v.doc_uuid, v.name, v.size_bytes, v.mime, a.obj_key
			FROM doc_file_versions v JOIN attachments a ON a.id = v.attachment_id
			WHERE v.doc_uuid = ANY($1::uuid[])
			ORDER BY v.doc_uuid, v.n DESC
		), att AS (
			INSERT INTO attachments (id, obj_key, src_key, src_value, created_by)
			SELECT p.att, s.obj_key, $4, p.dst::text, $5::uuid
			FROM pairs p JOIN src s ON s.doc_uuid = p.src
			RETURNING id
		)
		INSERT INTO doc_file_versions (doc_uuid, n, attachment_id, name, size_bytes, mime, created_by)
		SELECT p.dst, 1, p.att, s.name, s.size_bytes, s.mime, $5::uuid
		FROM pairs p JOIN src s ON s.doc_uuid = p.src JOIN att ON att.id = p.att`,
		uuidTexts(from), uuidTexts(to), uuidTexts(ids), postgresStruct.ATTACHMENT_SRC_DOC, by.String())
}
