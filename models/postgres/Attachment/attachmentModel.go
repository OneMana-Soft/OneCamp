package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

//CREATE TABLE IF NOT EXISTS attachment(
//"id" uuid PRIMARY KEY NOT NULL DEFAULT uuid_generate_v4(),
//"project_id" uuid REFERENCES teams(id),
//"obj_key" varchar NOT NULL,
//"created_by" uuid REFERENCES users(id),
//"created_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"updated_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW(),
//"deleted_at" TIMESTAMP WITH TIME ZONE DEFAULT NOW()
//);

type Attachment struct {
	Uuid      uuid.UUID `gorm:"primaryKey;type:uuid;default:uuid_generate_v4()"`
	ObjKey    string
	SrcValue  string
	SrcKey    string
	CreatedBy uuid.UUID
	CreatedAt time.Time
	DeletedAt time.Time
}

func CreateAttachment(query string, attachmentUUID uuid.UUID, objKey string, srcKey string, srcValue string, createdByUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		attachmentUUID,
		objKey,
		srcKey,
		srcValue,
		createdByUUID,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateAttachment Failed to create new attachment err: %+v",
			err)
		return
	}

	return
}

func CreateAttachmentForChannelsAndChats(query string, args ...interface{}) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateAttachmentForChannelsAndChats Failed to create new attachments err: %+v, query: %s",
			err, query)
		return err
	}
	return nil
}

//func CheckIfTaskExistByTaskNameAndProjectUUID(query string, projectName string, teamId uuid.UUID) (exist bool, err error) {
//	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
//	defer cancel()
//	err = postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, projectName, teamId).Scan(&exist)
//	if err != nil {
//		helpers.LogErrorWithContext(ctx,
//			"models/CheckIfTaskExistByTaskNameAndTeamUUID Failed to check if project name exist in given team ID err: %+v",
//			err)
//		return
//	}
//
//	return
//}

func GetAttachmentByObjUUID(query string, objUUID string, srcKey string) (returnAttachment *Attachment, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, objUUID, srcKey)
	defer func() {
		cancel()
	}()

	var createdAt sql.NullTime
	var deletedAt sql.NullTime

	var attachment Attachment

	err = row.Scan(
		&attachment.Uuid,
		&attachment.ObjKey,
		&attachment.SrcKey,
		&attachment.SrcValue,
		&attachment.CreatedBy,
		&createdAt,
		&deletedAt,
	)

	if createdAt.Valid {
		attachment.CreatedAt = createdAt.Time
	}

	if deletedAt.Valid {
		attachment.DeletedAt = deletedAt.Time
	}

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetAttachmentByObjUUID Failed to get attachment from postgres err: %+v",
			err)
		return
	}

	returnAttachment = &attachment

	return
}

// FilesOfPlace is which of ids are files of the place src (src_key, src_value)
// that aren't deleted: a write naming files of a place checks them here.
func FilesOfPlace(ctx context.Context, srcKey, srcValue string, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	var want []uuid.UUID
	for _, id := range ids {
		if u, err := uuid.Parse(id); err == nil {
			want = append(want, u)
		}
	}
	if len(want) == 0 {
		return out, nil
	}
	found, err := pgexec.Query(ctx, `SELECT id FROM attachments
		WHERE id = ANY($1) AND src_key = $2 AND src_value = $3 AND deleted_at IS NULL`,
		func(r pgexec.Row) (id uuid.UUID, err error) {
			err = r.Scan(&id)
			return id, err
		}, pq.Array(want), srcKey, srcValue)
	if err != nil {
		return nil, err
	}
	for _, id := range found {
		out[id.String()] = true
	}
	return out, nil
}
