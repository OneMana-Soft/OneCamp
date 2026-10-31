package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/models/pgexec"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Upload is a visitor's file waiting to be sent with a form (migration 494):
// kept under the claim their page holds (its SHA-256, never the token) for
// one question.
type Upload struct {
	ID       uuid.UUID
	Form     uuid.UUID
	Claim    string
	Question string
	// Key is the file's key in the uploads bucket, as attachments.obj_key
	// keeps it.
	Key         string
	Name        string
	ContentType string
	Size        int64
	// Tries is how many sweeps couldn't remove its object (Unsent only).
	Tries int
}

const uploadColumns = `id, form_id, claim, question, obj_key, file_name, content_type, size`

func scanUpload(r pgexec.Row) (Upload, error) {
	var u Upload
	err := r.Scan(&u.ID, &u.Form, &u.Claim, &u.Question, &u.Key, &u.Name, &u.ContentType, &u.Size)
	return u, err
}

// AddUpload keeps a waiting file, unless its claim already has max files
// waiting for the question: false then, and nothing is kept.
func AddUpload(ctx context.Context, u Upload, max int) (bool, error) {
	n, err := pgexec.Exec(ctx, `
		INSERT INTO form_uploads (`+uploadColumns+`)
		SELECT $1::uuid, $2::uuid, $3::varchar, $4::varchar, $5::varchar, $6::varchar, $7::varchar, $8::bigint
		 WHERE (SELECT COUNT(*) FROM form_uploads WHERE form_id = $2::uuid AND claim = $3::varchar AND question = $4::varchar AND claimed_at IS NULL) < $9`,
		u.ID, u.Form, u.Claim, u.Question, u.Key, u.Name, u.ContentType, u.Size, max)
	return n > 0, err
}

// Waiting is how many files a claim has waiting for a question.
func Waiting(ctx context.Context, form uuid.UUID, claim, question string) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM form_uploads WHERE form_id = $1 AND claim = $2 AND question = $3 AND claimed_at IS NULL`,
		form, claim, question)
}

// UploadsSince is how many files a form took since a moment, sent or not:
// the brake on a form's files being flooded.
func UploadsSince(ctx context.Context, form uuid.UUID, since time.Time) (int, error) {
	return pgexec.Scalar[int](ctx, `SELECT COUNT(*) FROM form_uploads WHERE form_id = $1 AND created_at > $2`, form, since)
}

// Claim takes a submission's files: those of ids waiting under the form and
// the claim, each taken once, and each made an attachment of the project
// owned by owner, in the same statement, so a file is either still waiting or
// the project's. It answers the files it took.
func Claim(ctx context.Context, form uuid.UUID, claim string, ids []uuid.UUID, project, owner uuid.UUID) ([]Upload, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `
		WITH taken AS (
			UPDATE form_uploads SET claimed_at = NOW()
			 WHERE form_id = $1 AND claim = $2 AND id = ANY($3::uuid[]) AND claimed_at IS NULL
			RETURNING `+uploadColumns+`
		), placed AS (
			INSERT INTO attachments (id, obj_key, src_key, src_value, created_by)
			SELECT id, obj_key, $4::varchar, $5::varchar, $6::uuid FROM taken
		)
		SELECT `+uploadColumns+` FROM taken`,
		scanUpload, form, claim, ids, postgresStruct.ATTACHMENT_SRC_PROJECT, project.String(), owner)
}

// Release puts files Claim took back to waiting, and takes away the project's
// attachments it made of them, when their submission couldn't be filed: the
// visitor can send them again.
func Release(ctx context.Context, ids []uuid.UUID, project uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := pgexec.Exec(ctx, `
		WITH back AS (
			UPDATE form_uploads SET claimed_at = NULL WHERE id = ANY($1::uuid[]) AND claimed_at IS NOT NULL RETURNING id
		)
		DELETE FROM attachments WHERE id IN (SELECT id FROM back) AND src_key = $2 AND src_value = $3`,
		ids, postgresStruct.ATTACHMENT_SRC_PROJECT, project.String())
	return err
}

// Unsent is up to n files that waited since before and were never sent, and
// aren't waiting for a retry after now, the oldest first: the sweep removes
// their objects, then forgets them (Forget); one whose removal failed waits
// for its retry_at (Tried), so it never holds the rest.
func Unsent(ctx context.Context, before, now time.Time, n int) ([]Upload, error) {
	return pgexec.Query(ctx, `SELECT `+uploadColumns+`, purge_tries FROM form_uploads
		WHERE created_at < $1 AND claimed_at IS NULL AND (retry_at IS NULL OR retry_at <= $2)
		ORDER BY created_at LIMIT $3`, func(r pgexec.Row) (Upload, error) {
		var u Upload
		err := r.Scan(&u.ID, &u.Form, &u.Claim, &u.Question, &u.Key, &u.Name, &u.ContentType, &u.Size, &u.Tries)
		return u, err
	}, before, now, n)
}

// Tried records one more sweep that couldn't remove the objects of ids, each
// to be tried again at its at, and answers the ones that have now failed at
// least giveUp times.
func Tried(ctx context.Context, ids []uuid.UUID, at []time.Time, giveUp int) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `WITH tried AS (
			UPDATE form_uploads u SET purge_tries = u.purge_tries + 1, retry_at = r.at
			  FROM unnest($1::uuid[], $2::timestamptz[]) AS r(id, at) WHERE u.id = r.id
			RETURNING u.id, u.purge_tries
		) SELECT id FROM tried WHERE purge_tries >= $3`, func(r pgexec.Row) (uuid.UUID, error) {
		var id uuid.UUID
		err := r.Scan(&id)
		return id, err
	}, ids, pq.Array(at), giveUp)
}

// UnsentBytes is the bytes of files waiting unsent for form, and for every
// form: the room a stranger's files take before anyone sends them.
func UnsentBytes(ctx context.Context, form uuid.UUID) (forForm, forAll int64, err error) {
	got, _, err := pgexec.QueryOne(ctx, `SELECT COALESCE(SUM(size) FILTER (WHERE form_id = $1), 0), COALESCE(SUM(size), 0)
		FROM form_uploads WHERE claimed_at IS NULL`, func(r pgexec.Row) ([2]int64, error) {
		var b [2]int64
		err := r.Scan(&b[0], &b[1])
		return b, err
	}, form)
	return got[0], got[1], err
}

// Forget deletes the waiting files ids, whose objects are gone, and every sent
// file older than before: a sent file's object is the project's attachment by
// then, and stays.
func Forget(ctx context.Context, ids []uuid.UUID, before time.Time) error {
	_, err := pgexec.Exec(ctx, `DELETE FROM form_uploads
		WHERE (id = ANY($1::uuid[]) AND claimed_at IS NULL) OR (claimed_at IS NOT NULL AND created_at < $2)`, ids, before)
	return err
}
