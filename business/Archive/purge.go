package business

// Permanent removal of archived files and recordings.
//
// Archiving hides; it keeps every byte. That is right for messages and tasks,
// which are small and which people ask to have back. It is wrong as the only
// option for files and recordings, which are where the disk goes, and which
// the workspace owner may need gone for good: a leaver's uploads, a recording
// that should not exist, or simply room. purge_after_days is the owner's
// decision, per policy, off by default. Once set, anything archived for that
// many days is removed from storage, the tables, the graph and the index, and
// cannot be restored. The count and the bytes are kept so the settings page
// can say what has gone.
//
// Only attachments and recordings can be purged. The other entity types are
// text; their graph edges reach into everything else, and deleting them for
// good is a different piece of work. The setting is refused for them rather
// than accepted and ignored.
//
// ORDER OF OPERATIONS. Bytes first, then the row, then the graph and the
// index. If the bytes cannot be removed the row stays and the next hour tries
// again; a row is never deleted while its file remains, because a row is how
// anyone would find the file to remove it. The other way round would be a
// file nothing names, on a disk the customer is told is theirs.

import (
	"context"
	"fmt"
	"time"

	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/minio/minio-go/v7"
)

const (
	// PurgeMinDays is the least an owner may set: a week to notice an
	// archive that was a mistake and restore it.
	PurgeMinDays = 7
	// PurgeMaxDays matches retention_days' ceiling.
	PurgeMaxDays = 3650
	// purgeBatch is per policy per hour. A large backlog drains over days
	// rather than in one long pass that holds the disk and the database.
	purgeBatch = 200
)

// purgeSupported says which entity types hold bytes a purge can free.
func purgeSupported(entityType string) bool {
	return entityType == "attachments" || entityType == "recordings"
}

// validatePurgeAfterDays is the rule behind the setting: 0 is always fine
// (keep forever); anything else needs a type that can be purged and a value
// within the same bounds as retention.
func validatePurgeAfterDays(entityType string, days int) error {
	if days == 0 {
		return nil
	}
	if !purgeSupported(entityType) {
		return fmt.Errorf("purge_after_days applies to attachments and recordings only; %s stays archived", entityType)
	}
	if days < PurgeMinDays {
		return fmt.Errorf("purge_after_days must be at least %d", PurgeMinDays)
	}
	if days > PurgeMaxDays {
		return fmt.Errorf("purge_after_days must be at most %d", PurgeMaxDays)
	}
	return nil
}

// purgeCutoff is the moment before which an archived item is due. The second
// value is false when the policy does not purge at all.
func purgeCutoff(p *ArchivePolicy, now time.Time) (time.Time, bool) {
	if p == nil || p.PurgeAfterDays <= 0 || !purgeSupported(p.EntityType) {
		return time.Time{}, false
	}
	return now.Add(-time.Duration(p.PurgeAfterDays) * 24 * time.Hour), true
}

// attachmentObject is where an attachment row's bytes live: every upload path
// (chat, import, Slack) writes under this prefix and stores the rest as
// obj_key. Kept as one function so the purge and the uploads cannot drift.
func attachmentObject(objKey string) string {
	return "userFileUpload/" + objKey
}

// PurgeResult is what one pass over one policy did.
type PurgeResult struct {
	Removed int64 // items gone for good
	Bytes   int64 // what they weighed
	Kept    int   // rows dropped whose file another live row still shows
	Failed  int   // left for the next pass
}

// The seams. Production wiring is below; tests replace what they need.
var (
	removeObject                 = removeObjectFromStorage
	listArchivedAttachments      = attachmentDomain.ListAttachmentsArchivedBefore
	objKeyHeldElsewhere          = attachmentDomain.ObjKeyHeldElsewhere
	deleteArchivedAttachmentRows = attachmentDomain.DeleteArchivedAttachmentRows
	deleteAttachmentsEverywhere  = attachmentDomain.DeleteAttachmentsEverywhere
	listArchivedRecordings       = recordingDomain.ListRecordingsArchivedBefore
	deleteRecordingNodes         = recordingDomain.DeleteRecordingNodes
	recordPurge                  = recordPurgeInPolicy
)

// removeObjectFromStorage removes one object and reports its size. An object
// that is already absent counts as removed with no size: the row it belonged
// to must still go, or it would be tried forever.
func removeObjectFromStorage(ctx context.Context, bucket, key string) (int64, error) {
	var size int64
	if st, err := minioInit.MinioClient.StatObject(ctx, bucket, key, minio.StatObjectOptions{}); err == nil {
		size = st.Size
	} else if isNoSuchKey(err) {
		return 0, nil
	} else {
		return 0, err
	}
	if err := minioInit.MinioClient.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}); err != nil && !isNoSuchKey(err) {
		return 0, err
	}
	return size, nil
}

func isNoSuchKey(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == 404
}

// recordPurgeInPolicy adds what a pass removed to the policy's running total.
func recordPurgeInPolicy(ctx context.Context, entityType string, removed, bytes int64) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE archive_policies
		SET purged_count = purged_count + $1, purged_bytes = purged_bytes + $2, last_purged_at = NOW()
		WHERE entity_type = $3`, removed, bytes, entityType)
	return err
}

// RunPurges runs one pass for every policy that purges. Each policy is its
// own pass with its own error: one type failing does not stop the other.
func RunPurges(ctx context.Context, policies []*ArchivePolicy, now time.Time) {
	for _, p := range policies {
		cutoff, ok := purgeCutoff(p, now)
		if !ok {
			continue
		}
		var res PurgeResult
		var err error
		switch p.EntityType {
		case "attachments":
			res, err = purgeAttachments(ctx, cutoff)
		case "recordings":
			res, err = purgeRecordings(ctx, cutoff)
		}
		if err != nil {
			helpers.LogErrorWithContext(ctx, "purge %s: %v", p.EntityType, err)
		}
		if res.Removed == 0 && res.Kept == 0 && res.Failed == 0 {
			continue
		}
		if err := recordPurge(ctx, p.EntityType, res.Removed, res.Bytes); err != nil {
			helpers.LogErrorWithContext(ctx, "purge %s: recording the total: %v", p.EntityType, err)
		}
		helpers.MessageLogs.InfoLog.Printf("purge %s: removed %d for good (%d bytes), %d rows dropped whose file is still shown elsewhere, %d left for next pass",
			p.EntityType, res.Removed, res.Bytes, res.Kept, res.Failed)
	}
}

// purgeAttachments removes attachments archived before the cutoff.
//
// One file can be several rows (an upload sent to three channels). The bytes
// go only when no row outside the cutoff still shows them; rows inside the
// cutoff go either way, and the bytes follow when the last one does.
func purgeAttachments(ctx context.Context, cutoff time.Time) (PurgeResult, error) {
	var res PurgeResult
	due, err := listArchivedAttachments(ctx, cutoff, purgeBatch)
	if err != nil {
		return res, err
	}
	bucket := helpers.UserUploadBucket()
	done := map[string]bool{}
	for _, a := range due {
		if done[a.ObjKey] {
			continue
		}
		done[a.ObjKey] = true
		held, err := objKeyHeldElsewhere(ctx, a.ObjKey, cutoff)
		if err != nil {
			res.Failed++
			continue
		}
		var size int64
		if !held {
			size, err = removeObject(ctx, bucket, attachmentObject(a.ObjKey))
			if err != nil {
				helpers.LogErrorWithContext(ctx, "purge attachments: removing %s: %v", a.ObjKey, err)
				res.Failed++
				continue
			}
		}
		ids, err := deleteArchivedAttachmentRows(ctx, a.ObjKey, cutoff)
		if err != nil {
			// The bytes may be gone and the row not. Next pass: StatObject
			// says absent, size 0, and the row goes then.
			res.Failed++
			continue
		}
		deleteAttachmentsEverywhere(ctx, ids)
		if held {
			res.Kept += len(ids)
		} else {
			res.Removed += int64(len(ids))
			res.Bytes += size
		}
	}
	return res, nil
}

// purgeRecordings removes recordings archived before the cutoff. A recording
// is one node and one file; nothing shares either.
func purgeRecordings(ctx context.Context, cutoff time.Time) (PurgeResult, error) {
	var res PurgeResult
	due, err := listArchivedRecordings(ctx, cutoff, purgeBatch)
	if err != nil {
		return res, err
	}
	bucket := helpers.UserUploadBucket()
	for _, r := range due {
		var size int64
		if r.ObjectKey != "" {
			size, err = removeObject(ctx, bucket, r.ObjectKey)
			if err != nil {
				helpers.LogErrorWithContext(ctx, "purge recordings: removing %s: %v", r.EgressId, err)
				res.Failed++
				continue
			}
		}
		if err := deleteRecordingNodes(ctx, r); err != nil {
			res.Failed++
			continue
		}
		res.Removed++
		res.Bytes += size
	}
	return res, nil
}
