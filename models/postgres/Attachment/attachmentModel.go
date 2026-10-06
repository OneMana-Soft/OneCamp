package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/lib/pq"

	"github.com/google/uuid"
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

func UpdateAttachmentDeletedTimeByUUID(query string, deleteTime *time.Time, attachmentUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		deleteTime,
		attachmentUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateAttachmentDeletedTimeByUUID Failed to update attachment's delete time err: %+v",
			err)
		return
	}

	return
}

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

func GetAttachmentByUUID(query string, uuid uuid.UUID) (attachment *Attachment, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var attachmentInfo Attachment
	var createdAt sql.NullTime
	var deletedAt sql.NullTime

	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, uuid)
	err = row.Scan(
		&attachmentInfo.Uuid,
		&attachmentInfo.ObjKey,
		&attachmentInfo.SrcKey,
		&attachmentInfo.SrcValue,
		&attachmentInfo.CreatedBy,
		&createdAt,
		&deletedAt,
	)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		helpers.LogErrorWithContext(ctx,
			"models/GetCommentByUUID Failed to get comment err: %+v",
			err)
		return
	}

	if createdAt.Valid {
		attachmentInfo.CreatedAt = createdAt.Time
	}

	if deletedAt.Valid {
		attachmentInfo.DeletedAt = deletedAt.Time
	}

	return &attachmentInfo, nil

}

func UpdateAttachmentNameByAttachmentUUID(query string, TeamName string, currentTime time.Time, projectUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		TeamName,
		currentTime,
		projectUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateAttachmentNameByAttachmentUUID Failed to update attachment err: %+v",
			err)
		return
	}

	return
}

func UpdateAttachmentDeletedTimeToNullByUUID(query string, updateTime *time.Time, attachmentUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		updateTime,
		attachmentUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateAttachmentDeletedTimeToNullByUUID Failed to update attachment's delete time to null err: %+v",
			err)
		return
	}

	return
}

func GetAttachmentsByObjUUIDs(query string, objUUIDs []string) (attachments []*Attachment, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, query, pq.Array(objUUIDs))
	defer func() {
		cancel()
		rows.Close()
	}()

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/GetAttachmentsByObjUUIDs Failed to get attachments list from postgres err: %+v",
			err)
		return
	}

	for rows.Next() {
		var createdAt sql.NullTime
		var deletedAt sql.NullTime

		var attachment Attachment
		err = rows.Scan(
			&attachment.Uuid,
			&attachment.ObjKey,
			&attachment.CreatedBy,
			&createdAt,
			&deletedAt,
		)
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/GetAttachmentsByObjUUIDs Failed to get attachment info from postgres err: %+v",
				err)
			return
		}

		if createdAt.Valid {
			attachment.CreatedAt = createdAt.Time
		}

		if deletedAt.Valid {
			attachment.DeletedAt = deletedAt.Time
		}

		attachments = append(attachments, &attachment)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err = rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Attachment rows iteration failed err: %+v", err)
		return
	}

	return
}
