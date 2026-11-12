package models

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Tag is a repository's tag a release goes out as: the repository
// ("owner/name", lower case), the tag, its release's name, and its commit.
type Tag struct {
	Repo, Name, Release, SHA string
}

// Shipped is what a tag did to a project's release.
type Shipped struct {
	Release Release
	// Since is the tag the repository last shipped in the project before this
	// one ("" for none): its commits since are the release's.
	Since string
	// Again is a tag the release had already shipped as, at the same commit:
	// a second delivery, or a tag's create and its GitHub release both.
	Again bool
}

// Shipping is what ShipTag would answer for t in project now, read without
// its lock and without a write: a code host reads the tag's tasks from it
// before the ship, and ShipTag reads it again under the lock.
func Shipping(ctx context.Context, project uuid.UUID, t Tag) (Shipped, error) {
	s, _, err := tagState(ctx, project, t)
	return s, err
}

// tagState is what t finds in project: the tag the repository last shipped
// there before it (Since), and the live release already shipped as it at
// its commit (Again), else the one it ships (found: the live release
// shipped as it at another commit, else the live one named it that no tag
// has shipped).
func tagState(ctx context.Context, project uuid.UUID, t Tag) (Shipped, bool, error) {
	since, _, err := pgexec.QueryOne(ctx, `SELECT tag FROM releases WHERE project_uuid = $1 AND repo = $2 AND tag <> $3
		AND state = 'released' AND `+softDelete.Live("")+` ORDER BY released_at DESC NULLS LAST, created_at DESC LIMIT 1`,
		pgexec.Text, project, t.Repo, t.Name)
	if err != nil {
		return Shipped{}, false, err
	}
	r, found, err := pgexec.QueryOne(ctx, `SELECT `+columns+` FROM releases WHERE project_uuid = $1 AND `+softDelete.Live("")+`
		AND ((repo = $2 AND tag = $3) OR (tag IS NULL AND lower(name) = lower($4))) ORDER BY tag IS NULL LIMIT 1`,
		scan, project, t.Repo, t.Name, t.Release)
	if err != nil {
		return Shipped{}, false, err
	}
	again := found && r.Tag == t.Name && r.TagSHA == t.SHA && r.State == Released
	return Shipped{Release: r, Since: since, Again: again}, found, nil
}

// ShipTag makes or ships project's release for tag, under the project's
// releases lock so two deliveries of one tag can't make two: the live
// release already shipped as the tag, else the live one named it that no
// tag has shipped, else a new one, released now, by whom. ErrTaken: a live
// release of the project has the name and another tag. Inside a caller's
// transaction the lock is held until it ends.
func ShipTag(ctx context.Context, project uuid.UUID, t Tag, by uuid.UUID) (Shipped, error) {
	var out Shipped
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "releases:"+project.String()); err != nil {
			return err
		}
		s, found, err := tagState(ctx, project, t)
		out = s
		switch {
		case err != nil || s.Again:
			return err
		case found:
			out.Release, _, err = pgexec.QueryOne(ctx, `UPDATE releases SET repo = $2, tag = $3, tag_sha = $4, state = 'released',
				released_at = COALESCE(released_at, NOW()), updated_at = NOW() WHERE id = $1 RETURNING `+columns, scan, s.Release.ID, t.Repo, t.Name, t.SHA)
			return err
		}
		out.Release, _, err = pgexec.QueryOne(ctx, `INSERT INTO releases (project_uuid, name, state, released_at, repo, tag, tag_sha, created_by)
			VALUES ($1, $2, 'released', NOW(), $3, $4, $5, $6) RETURNING `+columns, scan, project, t.Release, t.Repo, t.Name, t.SHA, softDelete.By(by))
		return err
	})
	if helpers.IsUniqueViolation(err) {
		return Shipped{}, ErrTaken
	}
	return out, err
}

// KeepStage keeps s on each live release of projects that s.Repo shipped as
// the tag ref or at the commit s.SHA, a report older than the one kept
// changing nothing, and answers the releases whose stage it changed, in one
// write. The selected values are cast: a parameter both selected and
// compared ($3, $8) is deduced text by the comparison and varchar by the
// column, which Postgres refuses as inconsistent.
func KeepStage(ctx context.Context, projects []uuid.UUID, ref string, s Stage) ([]uuid.UUID, error) {
	if len(projects) == 0 {
		return nil, nil
	}
	return pgexec.Query(ctx, `INSERT INTO release_stages (release_id, provider, repo, kind, name, status, sha, url, reported_at)
		SELECT r.id, $4::text, $3::text, $5::text, $6::text, $7::text, $8::text, $9::text, $10::timestamptz FROM releases r
		WHERE r.project_uuid = ANY($1::uuid[]) AND r.repo = $3 AND `+softDelete.Live("r")+`
			AND ((r.tag_sha = $8 AND $8 <> '') OR (r.tag = $2 AND $2 <> ''))
		ON CONFLICT (release_id, provider, repo, kind, name) DO UPDATE SET status = EXCLUDED.status, sha = EXCLUDED.sha,
			url = EXCLUDED.url, reported_at = EXCLUDED.reported_at
		WHERE release_stages.reported_at <= EXCLUDED.reported_at
		RETURNING release_id`, func(r pgexec.Row) (uuid.UUID, error) {
		var id uuid.UUID
		err := r.Scan(&id)
		return id, err
	}, pq.Array(projects), ref, s.Repo, s.Provider, s.Kind, s.Name, s.Status, s.SHA, s.URL, s.ReportedAt)
}
