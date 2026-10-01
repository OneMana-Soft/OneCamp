package business

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	postDomain "github.com/akashc777/OneCamp/domain/Post"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// SlackFileLinkWindow is how long Slack keeps an export's file links alive.
//
// Exported here because the plan needs the same number: an export older than
// this will import every message and silently fetch no attachments, and the
// only place that clock was written down was this comment. Approximate by
// nature, so the plan warns rather than refuses.
const SlackFileLinkWindow = 90 * 24 * time.Hour

// Slack file URLs in workspace exports are public for ~SlackFileLinkWindow from
// the export date but rate-limit aggressively. We use a single shared HTTP
// client with a sane timeout and back off on 429s.
//
// SSRF defence: an attacker who can craft a malicious export ZIP can
// put arbitrary URLs in url_private_download (e.g. internal metadata
// endpoints, RFC1918 IPs, link-local). We use the canonical
// SSRFSafeClient which:
//   - rejects any URL whose hostname resolves to a loopback / link-
//     local / private / multicast IP
//   - re-resolves at dial time to defeat DNS rebinding
//   - blocks redirects to blocked IPs
//
// Operators who genuinely need to reach an internal Slack mirror can
// add the relevant host to the allowlist via SLACK_FILE_HOST_ALLOWLIST
// (CSV); see ValidateSlackFetchURL.
//
// Slack's own CDN rotates IPs and is always public so the SSRF guard
// has no real effect on legitimate traffic.
//
// The worker streams the response body straight into MinIO; bytes never
// touch local disk. PutObject supports an io.Reader of unknown size by
// passing -1, which makes minio-go switch to multipart upload — perfect
// for files we don't know the exact size of (Slack's `size` field can
// be missing for older exports).
var slackHTTPClient = func() *http.Client {
	c := helpers.SSRFSafeClient(false)
	c.Timeout = 5 * time.Minute
	return c
}()

const (
	defaultMaxFileBytes int64 = 1 << 30 // 1 GB cap per file
	maxFileRetries            = 4
)

// processFileChunk downloads a Slack-hosted file to MinIO and creates
// an Attachment row consistent with the OneCamp native upload pipeline.
//
// Consistency contract — all four stores must agree on the attachment:
//
//	MinIO:        object at "userFileUpload/<importingAdminUUID>/<attUUID>_<name>"
//	              (same key shape as UploadUserFile so GetFileURLByObjectName works)
//	Postgres:     attachments row with obj_key=<importingAdminUUID>/<attUUID>_<name>
//	              (relative path — this is what GetFileURLByObjectName prepends to)
//	              and (src_key, src_value) per conversation type:
//	                channel/private group: ("channel",  channel_uuid)
//	                1:1 DM:                ("chat",     grouping_id)
//	                MPIM/group chat:       ("grpChat",  grouping_id)
//	Dgraph:       DgraphAttachment node + MediaObj edge from parent post/chat
//	OpenSearch:   attachments index doc with denormalised parent fields
//
// The Dgraph + OpenSearch writes are done by retrofitFileToParent below
// AFTER the parent post/chat has been imported. This keeps the two-pass
// ordering simple: file_worker writes everything except the parent edge,
// then the orchestrator's "files" stage retrofits the Dgraph edge.
//
// Failure modes:
//   - 4xx from Slack (gone, expired link)        → mark done with placeholder (no parent edge)
//   - 5xx / network error                        → mark failed (retried up to max_attempts)
//   - file > MaxFileBytes                        → mark done, log warning placeholder
//   - MinIO failure                              → mark failed (retried)
//   - rate-limited (429)                         → reset to pending without bumping attempts
//   - parent never imported (e.g., system msg)   → file uploaded, attachment row written
//     with channel-level src as fallback,
//     warning logged
func processFileChunk(ctx context.Context, chunk *importModels.Chunk,
	importingUser uuid.UUID, maxBytes int64) error {

	if chunk.ObjectKey == nil {
		return fmt.Errorf("file chunk missing object_key")
	}
	meta, err := parseFileChunkMeta(*chunk.ObjectKey)
	if err != nil {
		return fmt.Errorf("parse meta: %w", err)
	}

	if maxBytes <= 0 {
		maxBytes = defaultMaxFileBytes
	}

	// Idempotency: did we already create an attachment for this file?
	if existing, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
		importModels.EntityFile, meta.FileID); existing != uuid.Nil {
		return importModels.FinishChunk(ctx, chunk.Id, 1, nil)
	}

	// Hard size cap from Slack metadata. If size is missing we still
	// stream and bail mid-stream when limit hits.
	if meta.Size > 0 && meta.Size > maxBytes {
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_TOO_LARGE",
			fmt.Sprintf("file %s size %d exceeds cap %d; skipping", meta.Name, meta.Size, maxBytes),
			mustMarshal(map[string]interface{}{
				"name": meta.Name, "size": meta.Size, "url": meta.URL,
			}))
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.URL, nil)
	if err != nil {
		return err
	}

	// Pre-flight SSRF guard. The transport-level guard catches
	// DNS-rebinding at dial time, but ValidateOutboundURL also rejects
	// HTTP-only and bare-IP URLs early so a bad export fails fast.
	if _, err := helpers.ValidateOutboundURL(meta.URL, false); err != nil {
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_BLOCKED_URL",
			fmt.Sprintf("Slack file URL rejected by SSRF guard: %v", err),
			mustMarshal(map[string]interface{}{"url": meta.URL}))
		ph := uuid.New()
		_ = importModels.UpsertIdMapping(ctx, chunk.ImportId, importModels.EntityFile, meta.FileID,
			ph, nil, mustMarshal(map[string]interface{}{
				"placeholder": true, "reason": "ssrf_blocked",
			}))
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack file fetch: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusGone:
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_GONE",
			fmt.Sprintf("Slack returned %d for %s; placeholder created", resp.StatusCode, meta.Name),
			mustMarshal(map[string]interface{}{"url": meta.URL}))
		// Map to a placeholder uuid so re-runs don't re-fetch.
		ph := uuid.New()
		_ = importModels.UpsertIdMapping(ctx, chunk.ImportId, importModels.EntityFile, meta.FileID,
			ph, nil, mustMarshal(map[string]interface{}{
				"placeholder": true, "status_code": resp.StatusCode,
			}))
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)

	case resp.StatusCode == http.StatusTooManyRequests:
		// Respect Retry-After if present, else default 5s.
		retryAfter := 5 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryAfter):
		}
		_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "rate limited; retry-after honoured")
		return errReaperOnly

	case resp.StatusCode >= 400:
		return fmt.Errorf("slack returned %d", resp.StatusCode)
	}

	// MinIO key shape MUST match UploadUserFile so GetFileURLByObjectName
	// (which prepends "userFileUpload/" to whatever's in attachments.obj_key)
	// produces a valid presigned URL.
	bucketName := helpers.UserUploadBucket()
	attachmentUUID := uuid.New()
	sanitizedName := helpers.SanitizeUploadFileName(meta.Name)
	// objKey is what gets stored in the postgres attachments.obj_key column.
	// fullObjName is what gets uploaded to MinIO (prefixed with userFileUpload/).
	objKey := fmt.Sprintf("%s/%s_%s", importingUser, attachmentUUID, sanitizedName)
	fullObjName := fmt.Sprintf("%s/%s", "userFileUpload", objKey)

	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = helpers.GuessContentTypeFromName(meta.Name)
	}

	// Coerce Content-Type to a safe value. Slack's CDN often returns
	// the upstream-uploaded type verbatim; that's fine for images and
	// PDFs but lets a malicious workspace serve text/html or
	// image/svg+xml that browsers render inline. uploadsafe.SafeContentType
	// strips dangerous types to application/octet-stream; combined
	// with Content-Disposition: attachment below this neutralises
	// stored-XSS via imported attachments.
	contentType = uploadsafe.SafeContentType(contentType, filepath.Ext(meta.Name))
	disposition := uploadsafe.BuildDisposition(sanitizedName)

	// Stream-cap reader to maxBytes. If the upstream lies about size,
	// we cut off at the cap and mark a warning.
	body := io.LimitReader(resp.Body, maxBytes+1)

	// AV scan in parallel with the upload. We tee the body into both
	// MinIO (via PutObject on the pipe reader) and the AV scanner
	// (via the pipe's other branch) so we only read the network
	// once. If the scanner returns Infected we cancel the MinIO
	// write and refuse to map the attachment.
	uploadInfo, scanResult, err := uploadWithAVScan(ctx, bucketName, fullObjName, body,
		contentType, disposition)
	if err != nil {
		return fmt.Errorf("minio put: %w", err)
	}

	// AV verdict handling. Infected → roll back the MinIO upload and
	// log; the file is recorded as a placeholder so retries don't
	// loop. Unknown (scanner unreachable) → honour AVSCAN_FAIL_OPEN.
	if scanResult.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName, minio.RemoveObjectOptions{})
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityError, "FILE_INFECTED",
			fmt.Sprintf("AV scan detected %s in %s; file dropped", scanResult.Signature, meta.Name),
			mustMarshal(map[string]interface{}{
				"signature": scanResult.Signature, "name": meta.Name, "url": meta.URL,
			}))
		ph := uuid.New()
		_ = importModels.UpsertIdMapping(ctx, chunk.ImportId, importModels.EntityFile, meta.FileID,
			ph, nil, mustMarshal(map[string]interface{}{
				"placeholder": true, "reason": "av_infected", "signature": scanResult.Signature,
			}))
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	if uploadInfo.Size > maxBytes {
		_ = minioInit.MinioClient.RemoveObject(ctx, bucketName, fullObjName, minio.RemoveObjectOptions{})
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_TOO_LARGE_STREAM",
			fmt.Sprintf("file %s exceeded cap during stream; skipped", meta.Name), nil)
		return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
	}

	// Resolve parent context: which conversation does this file attach to?
	parentInfo, err := resolveFileParent(ctx, chunk, meta)
	if err != nil {
		// Defensive: file uploaded but parent unresolvable. Roll back the
		// MinIO upload so we don't leak storage.
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName, minio.RemoveObjectOptions{})
		return fmt.Errorf("resolve parent: %w", err)
	}

	if err := attachmentBusiness.CreateAttachment(ctx, attachmentUUID, objKey, importingUser,
		parentInfo.SrcKey, parentInfo.SrcValue); err != nil {
		// Roll back the MinIO upload so a retry doesn't leak orphans.
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName, minio.RemoveObjectOptions{})
		return fmt.Errorf("create attachment row: %w", err)
	}

	// Detect attachment type for the FE renderer. Same heuristic as
	// onecamp-fe/lib/utils/file/getAttachmentType.ts so the import-side
	// classification matches what a native upload would produce.
	attType := classifyAttachmentByName(meta.Name)
	rawType := contentType

	// Dgraph: write the standalone DgraphAttachment node. The MediaObj
	// edge that ties it to the parent post/chat is added below in
	// retrofitFileToParent.
	createdAt := slackTsToTime(meta.ParentTs)
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	dgAttachment := &dgraphStruct.DgraphAttachment{
		Uuid:      attachmentUUID.String(),
		FileName:  meta.Name,
		ObjectKey: objKey,
		Type:      attType,
		RawType:   rawType,
		Size:      int(uploadInfo.Size),
		CreatedAt: &createdAt,
		CreatedBy: &dgraphStruct.DgraphUser{Uuid: importingUser.String()},
		DType:     []string{"Attachment"},
	}
	if _, err := attachmentDomain.BulkAddAttachmentsToDgraph(ctx, []*dgraphStruct.DgraphAttachment{dgAttachment}); err != nil {
		// Non-fatal: the file is in MinIO and Postgres. Dgraph drift is
		// reconcileable. Log loudly so the operator can re-run.
		helpers.LogWarnWithContext(ctx,
			"SlackImport file %s: Dgraph attachment write failed (will not appear in UI until reconciled): %+v",
			attachmentUUID, err)
	}

	// Retrofit: attach this DgraphAttachment to the parent post/chat in
	// Dgraph, and write the OpenSearch attachments-index entry with all
	// denormalised parent fields. Both happen here (rather than a
	// separate stage) so a single chunk is the unit of consistency.
	if parentInfo.ParentUUID != uuid.Nil {
		if err := retrofitFileToParent(ctx, parentInfo, dgAttachment, uploadInfo.Size); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport file %s: parent retrofit failed: %+v", attachmentUUID, err)
		}
	} else {
		// Defensive log path. Should be rare — files only get scheduled
		// for messages whose parent was successfully imported.
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_PARENT_UNRESOLVED",
			"file uploaded and indexed but no parent message edge written",
			mustMarshal(map[string]interface{}{
				"obj_key": objKey, "channel_slack_id": chunk.ChannelSlackId,
			}))
	}

	return importModels.UpsertIdMappingThenFinish(ctx, chunk.ImportId, chunk.Id,
		importModels.EntityFile, meta.FileID, attachmentUUID,
		mustMarshal(map[string]interface{}{
			"name":         meta.Name,
			"obj_key":      objKey,
			"size":         uploadInfo.Size,
			"parent_uuid":  parentInfo.ParentUUID.String(),
			"parent_kind":  parentInfo.Kind,
			"channel_uuid": parentInfo.ChannelUUID.String(),
			"grouping_id":  parentInfo.GroupingId,
		}))
}

// fileParentInfo carries everything retrofitFileToParent needs to wire
// the dgraph edge and write the opensearch attachments doc. Resolved
// once per file in resolveFileParent.
type fileParentInfo struct {
	// SrcKey/SrcValue go into the postgres attachments row.
	SrcKey   string
	SrcValue string

	// Kind discriminates the retrofit pathway:
	//   "post"   — channel post (write Post.MediaObj edge, opensearch with channel_uuid+post_uuid)
	//   "chat"   — DM or MPIM (write Chat.MediaObj edge, opensearch with chat_uuid+chat_grp_id)
	//   "channel" — file referenced in a system message we kept as channel-level
	//               attachment with no post edge. Rare.
	Kind string

	// ParentUUID is the OneCamp UUID of the post/chat to attach to.
	ParentUUID uuid.UUID

	// ChannelUUID is set for channel posts; used for OpenSearch denorm.
	ChannelUUID uuid.UUID

	// ChannelName populated when Kind=="post", for OpenSearch denorm.
	ChannelName string

	// GroupingId is set for chat (DM/MPIM); used for OpenSearch denorm.
	GroupingId string

	// AuthorUUID/AuthorName populated for OpenSearch denorm of attachment_by_*.
	AuthorUUID string
	AuthorName string

	// MemberUUIDs are the OneCamp UUIDs of every participant in a DM
	// or MPIM. Set only when Kind=="chat"; used to populate
	// AttachmentChatParticipants in OpenSearch so cross-user search by
	// participant works the same as a native chat upload.
	MemberUUIDs []string
}

// resolveFileParent works out where this file's parent message lives.
// It looks up the slack DM/MPIM/channel id, then the parent message ts,
// then loads the minimal parent metadata needed for the retrofit.
func resolveFileParent(ctx context.Context, chunk *importModels.Chunk, meta *fileChunkMeta) (*fileParentInfo, error) {
	out := &fileParentInfo{
		SrcKey: postgressStruct.ATTACHMENT_SRC_CHANNEL,
	}
	if chunk.ChannelSlackId == nil {
		return out, fmt.Errorf("missing channel_slack_id")
	}
	slackId := *chunk.ChannelSlackId

	// Determine conversation kind: DM, MPIM, or channel.
	if dmUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityDM, slackId); dmUUID != uuid.Nil {
		out.Kind = "chat"
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_CHAT
		if md, _ := importModels.GetIdMapMetadata(ctx, chunk.ImportId, importModels.EntityDM, slackId); len(md) > 0 {
			var m struct {
				GroupingId string   `json:"grouping_id"`
				Members    []string `json:"members"`
			}
			_ = json.Unmarshal(md, &m)
			out.SrcValue = m.GroupingId
			out.GroupingId = m.GroupingId
			out.MemberUUIDs = m.Members
		}
	} else if mpimUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityMPIM, slackId); mpimUUID != uuid.Nil {
		out.Kind = "chat"
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_GRP_CHAT
		if md, _ := importModels.GetIdMapMetadata(ctx, chunk.ImportId, importModels.EntityMPIM, slackId); len(md) > 0 {
			var m struct {
				GroupingId string   `json:"grouping_id"`
				Members    []string `json:"members"`
			}
			_ = json.Unmarshal(md, &m)
			out.SrcValue = m.GroupingId
			out.GroupingId = m.GroupingId
			out.MemberUUIDs = m.Members
		}
	} else if channelUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityChannel, slackId); channelUUID != uuid.Nil {
		out.Kind = "post"
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_CHANNEL
		out.SrcValue = channelUUID.String()
		out.ChannelUUID = channelUUID
		// Channel name from id_map metadata so we don't hit Dgraph here.
		if md, _ := importModels.GetIdMapMetadata(ctx, chunk.ImportId, importModels.EntityChannel, slackId); len(md) > 0 {
			var m struct {
				FinalName string `json:"final_name"`
			}
			_ = json.Unmarshal(md, &m)
			out.ChannelName = m.FinalName
		}
	}

	if out.SrcValue == "" {
		return out, fmt.Errorf("could not resolve parent conversation for slack_id=%s", slackId)
	}

	// Resolve the parent message uuid (post for channels, chat for DM/MPIM).
	if meta.ParentTs != "" {
		if parentUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
			importModels.EntityMessage, meta.ParentTs); parentUUID != uuid.Nil {
			out.ParentUUID = parentUUID
		}
		// Resolve author for OpenSearch denorm. Cheap: one PG row.
		if md, _ := importModels.GetIdMapMetadata(ctx, chunk.ImportId,
			importModels.EntityMessage, meta.ParentTs); len(md) > 0 {
			var m struct {
				AuthorSlack string `json:"author_slack"`
			}
			if err := json.Unmarshal(md, &m); err == nil && m.AuthorSlack != "" {
				if authorUUID, _ := importModels.LookupIdMapping(ctx, chunk.ImportId,
					importModels.EntityUser, m.AuthorSlack); authorUUID != uuid.Nil {
					out.AuthorUUID = authorUUID.String()
					// Name is not strictly needed; OpenSearch denorm
					// degrades to empty if missing.
				}
			}
		}
	}

	return out, nil
}

// retrofitFileToParent stitches the imported attachment to its parent
// in Dgraph (the MediaObj edge) and writes the OpenSearch index entry.
//
// Why a single function: keeps the cross-store consistency atomic per
// file. If either store fails we log loudly; the operator can re-run
// the import (idempotent) or operate on the orphan via the admin UI.
func retrofitFileToParent(ctx context.Context, p *fileParentInfo, att *dgraphStruct.DgraphAttachment, size int64) error {
	switch p.Kind {
	case "post":
		// Dgraph: append MediaObj edge from Post → Attachment using a
		// query-by-uuid upsert. We reuse the existing attachment domain
		// helper rather than crafting raw mutations here.
		if err := postDomain.AddAttachmentToPostInDgraph(ctx, p.ParentUUID.String(), att); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport: AddAttachmentToPostInDgraph failed post=%s att=%s err=%+v",
				p.ParentUUID, att.Uuid, err)
		}
		// OpenSearch: attachment doc with denormalised parent fields.
		osAttachment := &openSearchStruct.OpenSearchAttachment{
			Uuid:                     att.Uuid,
			AttachmentByUserUuid:     p.AuthorUUID,
			AttachmentByUserFullName: p.AuthorName,
			AttachmentFileName:       att.FileName,
			AttachmentObjKey:         att.ObjectKey,
			AttachmentChannelUuid:    p.ChannelUUID.String(),
			AttachmentChannelName:    p.ChannelName,
			AttachmentPostUuid:       p.ParentUUID.String(),
			AttachmentCreatedAt:      safeUnix(att.CreatedAt),
			AttachmentDeletedAt:      nil,
		}
		go attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAttachment)
	case "chat":
		// Dgraph: append MediaObj edge from Chat → Attachment.
		if err := chatDomain.AddAttachmentToChatInDgraph(ctx, p.ParentUUID.String(), att); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport: AddAttachmentToChatInDgraph failed chat=%s att=%s err=%+v",
				p.ParentUUID, att.Uuid, err)
		}
		// OpenSearch. Mirror native CreateChatWithAttachmentsInOpenSearch
		// by populating attachment_chat_participants — without it the
		// "shared with me" / cross-user attachment search won't return
		// imported files for participants who aren't the uploader.
		osAttachment := &openSearchStruct.OpenSearchAttachment{
			Uuid:                           att.Uuid,
			AttachmentByUserUuid:           p.AuthorUUID,
			AttachmentByUserFullName:       p.AuthorName,
			AttachmentFileName:             att.FileName,
			AttachmentObjKey:               att.ObjectKey,
			AttachmentChatGrpId:            p.GroupingId,
			AttachmentChatUuid:             p.ParentUUID.String(),
			AttachmentChatFromUserUuid:     p.AuthorUUID,
			AttachmentChatFromUserFullName: p.AuthorName,
			AttachmentChatParticipants:     buildChatParticipants(ctx, p.MemberUUIDs),
			AttachmentCreatedAt:            safeUnix(att.CreatedAt),
			AttachmentDeletedAt:            nil,
		}
		go attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAttachment)
	default:
		// Channel-level attachment without a post edge. Just index for
		// search; the Dgraph node already exists.
		osAttachment := &openSearchStruct.OpenSearchAttachment{
			Uuid:                  att.Uuid,
			AttachmentFileName:    att.FileName,
			AttachmentObjKey:      att.ObjectKey,
			AttachmentChannelUuid: p.ChannelUUID.String(),
			AttachmentChannelName: p.ChannelName,
			AttachmentCreatedAt:   safeUnix(att.CreatedAt),
			AttachmentDeletedAt:   nil,
		}
		go attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAttachment)
	}
	return nil
}

// fileChunkMeta is the parsed form of object_key for ChunkFile entries.
// Encoded as JSON in the chunks.object_key column so URLs/filenames
// containing pipes, colons, ampersands, etc. survive the round-trip.
type fileChunkMeta struct {
	FileID   string `json:"file_id"`
	URL      string `json:"url"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	ParentTs string `json:"parent_ts"`
}

// parseFileChunkMeta inverts the encoding used in buildFileChunk.
// Tolerates the legacy pipe-delimited format from earlier iterations
// of the importer so a job partway through processing during the
// upgrade isn't stranded with chunks it can't parse.
func parseFileChunkMeta(s string) (*fileChunkMeta, error) {
	if s == "" {
		return nil, fmt.Errorf("empty file chunk meta")
	}
	out := &fileChunkMeta{}

	// JSON path: starts with '{'.
	if s[0] == '{' {
		if err := json.Unmarshal([]byte(s), out); err != nil {
			return nil, fmt.Errorf("invalid file chunk meta json: %w", err)
		}
		if out.FileID == "" || out.URL == "" {
			return nil, fmt.Errorf("invalid file chunk meta: missing file_id or url")
		}
		return out, nil
	}

	// Legacy pipe-delimited fallback.
	for _, part := range strings.Split(s, "|") {
		idx := strings.IndexByte(part, ':')
		if idx <= 0 {
			continue
		}
		key, val := part[:idx], part[idx+1:]
		switch key {
		case "file":
			out.FileID = val
		case "url":
			out.URL = val
		case "name":
			out.Name = val
		case "size":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				out.Size = n
			}
		case "parent":
			out.ParentTs = val
		}
	}
	if out.FileID == "" || out.URL == "" {
		return nil, fmt.Errorf("invalid file chunk meta")
	}
	return out, nil
}

// classifyAttachmentByName mirrors onecamp-fe/lib/utils/file/getAttachmentType.ts
// so an imported attachment renders the same as a natively uploaded one.
// Slack file `Mode` (e.g., "snippet", "post") is ignored — we classify by
// extension to match the FE.
func classifyAttachmentByName(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image"
	case ".pdf", ".txt", ".doc", ".docx", ".xls", ".xlsx", ".csv":
		return "document"
	case ".mp3", ".wav", ".ogg", ".m4a":
		return "audio"
	case ".mp4", ".mov", ".webm", ".avi", ".mkv":
		return "video"
	default:
		return "other"
	}
}

// safeUnix returns t.Unix() or 0 for a nil pointer. Tiny helper used in
// OpenSearch denormalisation. Kept separate from time.Now() so callers
// who pass zero-valued time get a stable zero rather than "now".
func safeUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

// buildChatParticipants loads the OpenSearchChatParticipants slice for
// every member uuid in the DM/MPIM grouping. Mirrors the loop in
// CreateChat / CreateChatForGroup so the imported attachment doc has
// the same shape as a natively-uploaded one.
//
// We tolerate per-user lookup failures: a missing participant degrades
// to fewer entries in the slice rather than failing the whole upload.
// Most realistic failure mode is a Dgraph soft-delete'd participant.
func buildChatParticipants(ctx context.Context, memberUUIDs []string) []*openSearchStruct.OpenSearchChatParticipants {
	if len(memberUUIDs) == 0 {
		return nil
	}
	users, err := userBusiness.GetDgraphUserInfoByUUIDs(ctx, memberUUIDs)
	if err != nil || len(users) == 0 {
		// Fall back to id-only entries; even without name/profile the
		// uuid is enough for permission filtering on attachment search.
		out := make([]*openSearchStruct.OpenSearchChatParticipants, 0, len(memberUUIDs))
		for _, u := range memberUUIDs {
			if u == "" {
				continue
			}
			out = append(out, &openSearchStruct.OpenSearchChatParticipants{Uuid: u})
		}
		return out
	}
	out := make([]*openSearchStruct.OpenSearchChatParticipants, 0, len(users))
	for _, u := range users {
		if u == nil || u.Uuid == "" {
			continue
		}
		out = append(out, &openSearchStruct.OpenSearchChatParticipants{
			Uuid:       u.Uuid,
			Name:       u.UserName,
			ProfileKey: u.ProfileKey,
		})
	}
	return out
}

// silence linter when helpers isn't otherwise used in this file.
var _ = helpers.RemoveHTMLTags

// uploadWithAVScan streams body into MinIO and (when an AV scanner is
// configured) into the scanner concurrently using io.Pipe. The scan
// result is returned alongside the MinIO UploadInfo.
//
// Concurrency / lifecycle:
//
//   - We start a goroutine that copies body into an io.MultiWriter
//     fan-out: one branch is the MinIO PutObject reader, the other
//     is the AV scanner reader.
//   - On any error in either branch we close the pipe so the
//     remaining branch returns immediately.
//   - When no scanner is configured (avscan.Default() returns nil)
//     we skip the tee entirely and PutObject straight from body —
//     no perf overhead.
//
// Behaviour on scanner error follows AVSCAN_FAIL_OPEN. With
// fail-open=true (default) the verdict is reported as VerdictUnknown
// and the upload is allowed; the caller can choose to log. With
// fail-open=false the function returns the scanner error and the
// caller treats it as a hard upload failure.
func uploadWithAVScan(ctx context.Context, bucket, objKey string, body io.Reader,
	contentType, disposition string) (minio.UploadInfo, avscan.Result, error) {
	return attachmentBusiness.SafeUploadToMinio(ctx, body, attachmentBusiness.SafeUploadOptions{
		Bucket:             bucket,
		ObjectName:         objKey,
		ContentType:        contentType,
		ContentDisposition: disposition,
		Size:               -1, // unknown size; MinIO uses multipart
	})
}

// keep errors import live for future error matching helpers (e.g.
// errors.Is on AV-specific errors).
var _ = errors.New
