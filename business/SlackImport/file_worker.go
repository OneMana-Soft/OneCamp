package business

import (
	"archive/zip"
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
	attachmentDomain "github.com/akashc777/OneCamp/domain/Attachment"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
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
//	Dgraph:       DgraphAttachment node, listed by its parent post/chat
//	OpenSearch:   attachments index doc with denormalised parent fields
//
// The files stage runs after the posts and chats are imported, so a file's
// node and its parent's edge to it are one upsert (attachmentDomain.LinkFile),
// and indexFile then writes its search entry.
//
// Failure modes:
//   - 4xx from Slack (gone, expired link)        → mark done with placeholder (no parent edge)
//   - 5xx / network error                        → mark failed (retried up to max_attempts)
//   - file > MaxFileBytes                        → mark done, log warning placeholder
//   - MinIO failure                              → mark failed (retried)
//   - a read of the import's maps fails          → mark failed (retried)
//   - rate-limited (429)                         → reset to pending without bumping attempts
//   - parent never imported (e.g., system msg)   → file uploaded, attachment row written
//     with channel-level src as fallback,
//     warning logged
func processFileChunk(ctx context.Context, src fileSource, chunk *importModels.Chunk,
	importingUser *userModels.UserInfo, maxBytes int64) error {

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

	// Idempotency: did we already create an attachment for this file? A read
	// that fails isn't "not yet": the file would be fetched and made again.
	existing, err := importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityFile, meta.FileID)
	if err != nil {
		return fmt.Errorf("reading what this import made for file %s: %w", meta.FileID, err)
	}
	if existing != uuid.Nil {
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

	body, served, finished, err := src.open(ctx, chunk, meta)
	if finished || err != nil {
		return err
	}
	defer body.Close()

	// MinIO key shape MUST match UploadUserFile, stored under the one name
	// every reader asks for (attachmentBusiness.ObjectName of
	// attachments.obj_key), so GetFileURLByObjectName presigns a valid URL.
	bucketName := helpers.UserUploadBucket()
	attachmentUUID := uuid.New()
	sanitizedName := helpers.SanitizeUploadFileName(meta.Name)
	// objKey is what gets stored in the postgres attachments.obj_key column.
	// fullObjName is what gets uploaded to MinIO (attachmentBusiness.ObjectName).
	objKey := fmt.Sprintf("%s/%s_%s", importingUser.UserPostgresInfo.Id, attachmentUUID, sanitizedName)
	fullObjName := attachmentBusiness.ObjectName(objKey)

	contentType := served
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
	limited := io.LimitReader(body, maxBytes+1)

	// AV scan in parallel with the upload. We tee the body into both
	// MinIO (via PutObject on the pipe reader) and the AV scanner
	// (via the pipe's other branch) so we only read the network
	// once. If the scanner returns Infected we cancel the MinIO
	// write and refuse to map the attachment.
	uploadInfo, scanResult, err := uploadWithAVScan(ctx, bucketName, fullObjName, limited,
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
		return filePlaceholder(ctx, chunk, meta, map[string]interface{}{"reason": "av_infected", "signature": scanResult.Signature})
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

	if err := attachmentBusiness.CreateAttachment(ctx, attachmentUUID, objKey, importingUser.UserPostgresInfo.Id,
		parentInfo.SrcKey, parentInfo.SrcValue); err != nil {
		// Roll back the MinIO upload so a retry doesn't leak orphans.
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName, minio.RemoveObjectOptions{})
		return fmt.Errorf("create attachment row: %w", err)
	}

	// Detect attachment type for the FE renderer. Same heuristic as
	// onecamp-fe/lib/utils/file/getAttachmentType.ts so the import-side
	// classification matches what a native upload would produce.
	attType := attachmentBusiness.KindOf(meta.Name)
	rawType := contentType

	// Dgraph: the file's node names its maker by the importer's own node.
	// A maker nested by uuid alone is a new, untyped user node, one per file.
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
		CreatedBy: &dgraphStruct.DgraphUser{Uid: importingUser.UserDgraphInfo.Uid},
		DType:     []string{"Attachment"},
	}
	// A graph write that fails is only logged: the file is in MinIO and
	// Postgres, and this import records it only as the chunk finishes, so a
	// chunk that ran again would keep it a second time.
	holderKind, holder := parentInfo.holder()
	if holder != uuid.Nil {
		// The node and its holder's edge to it in one upsert, so the holder
		// lists this one node.
		if err := attachmentDomain.LinkFile(ctx, holderKind, holder.String(), dgAttachment); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport file %s: linking it to its %s %s failed: %+v", attachmentUUID, holderKind, holder, err)
		}
		indexFile(ctx, parentInfo, dgAttachment)
	} else {
		if _, err := attachmentDomain.BulkAddAttachmentsToDgraph(ctx, []*dgraphStruct.DgraphAttachment{dgAttachment}); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport file %s: Dgraph attachment write failed: %+v", attachmentUUID, err)
		}
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
			"parent_uuid":  holder.String(),
			"parent_kind":  holderKind,
			"channel_uuid": parentInfo.ChannelUUID.String(),
			"grouping_id":  parentInfo.GroupingId,
		}))
}

// fileParentInfo carries everything the file's graph link and its
// opensearch attachments doc need. Resolved once per file in
// resolveFileParent.
type fileParentInfo struct {
	// SrcKey/SrcValue go into the postgres attachments row.
	SrcKey   string
	SrcValue string

	// Kind is the parent's kind, as LinkFile and indexFile take it:
	//   "post" — channel post (opensearch with channel_uuid+post_uuid)
	//   "chat" — DM or MPIM (opensearch with chat_uuid+chat_grp_id)
	Kind string

	// ParentUUID is the OneCamp UUID of the post/chat the file is under: a
	// thread reply's thread's, which its search entry is filed under.
	ParentUUID uuid.UUID

	// Comment is the thread reply the file came with, when it came with one:
	// the file hangs on the reply, as a reply's upload in the app does.
	Comment uuid.UUID

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

// holder is what the file hangs on in the graph, as LinkFile takes it: the
// reply it came with, else its post or chat. Nil while its post or chat
// isn't known.
func (p *fileParentInfo) holder() (kind string, id uuid.UUID) {
	switch {
	case p.ParentUUID == uuid.Nil:
		return p.Kind, uuid.Nil
	case p.Comment != uuid.Nil:
		return "comment", p.Comment
	}
	return p.Kind, p.ParentUUID
}

// resolveFileParent works out where this file's parent message lives.
// It looks up the slack DM/MPIM/channel id, then the parent message ts,
// then loads the minimal parent metadata the link and the search entry need.
func resolveFileParent(ctx context.Context, chunk *importModels.Chunk, meta *fileChunkMeta) (*fileParentInfo, error) {
	out := &fileParentInfo{
		SrcKey: postgressStruct.ATTACHMENT_SRC_CHANNEL,
	}
	if chunk.ChannelSlackId == nil {
		return out, fmt.Errorf("missing channel_slack_id")
	}
	slackId := *chunk.ChannelSlackId

	// Which conversation the file is in, as this import mapped its id. A read
	// that fails here or below is the file's error (its chunk is tried
	// again), never "no conversation" or "no parent": the file was filed on
	// its channel for good.
	kind, convUUID, err := importModels.LookupConversationMapping(ctx, chunk.ImportId, slackId)
	if err != nil {
		return out, fmt.Errorf("reading what this import made for conversation %s: %w", slackId, err)
	}
	switch kind {
	case importModels.EntityDM, importModels.EntityMPIM:
		out.Kind = "chat"
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_CHAT
		if kind == importModels.EntityMPIM {
			out.SrcKey = postgressStruct.ATTACHMENT_SRC_GRP_CHAT
		}
		var m struct {
			GroupingId string   `json:"grouping_id"`
			Members    []string `json:"members"`
		}
		if err := decodeIdMapMeta(ctx, chunk.ImportId, kind, slackId, &m); err != nil {
			return out, err
		}
		out.SrcValue = m.GroupingId
		out.GroupingId = m.GroupingId
		out.MemberUUIDs = m.Members
	case importModels.EntityChannel:
		out.Kind = "post"
		out.SrcKey = postgressStruct.ATTACHMENT_SRC_CHANNEL
		out.SrcValue = convUUID.String()
		out.ChannelUUID = convUUID
		// Channel name from id_map metadata so we don't hit Dgraph here.
		var m struct {
			FinalName string `json:"final_name"`
		}
		if err := decodeIdMapMeta(ctx, chunk.ImportId, kind, slackId, &m); err != nil {
			return out, err
		}
		out.ChannelName = m.FinalName
	}

	if out.SrcValue == "" {
		return out, fmt.Errorf("could not resolve parent conversation for slack_id=%s", slackId)
	}

	// Resolve the parent message uuid (post for channels, chat for DM/MPIM),
	// known by its conversation and its ts (msgkey.go). A thread reply was
	// made a comment under its thread's message, under the same key.
	if meta.ParentTs != "" {
		parent := messageKey(slackId, meta.ParentTs)
		entity := importModels.EntityMessage
		parentUUID, err := importModels.LookupIdMapping(ctx, chunk.ImportId, entity, parent)
		if err == nil && parentUUID == uuid.Nil {
			entity = importModels.EntityComment
			out.Comment, err = importModels.LookupIdMapping(ctx, chunk.ImportId, entity, parent)
		}
		if err != nil {
			return out, fmt.Errorf("reading what this import made for message %s: %w", parent, err)
		}
		out.ParentUUID = parentUUID
		// Resolve author for OpenSearch denorm. Cheap: one PG row.
		var m struct {
			AuthorSlack string `json:"author_slack"`
			ThreadTs    string `json:"thread_ts"`
		}
		if err := decodeIdMapMeta(ctx, chunk.ImportId, entity, parent, &m); err != nil {
			return out, err
		}
		if out.Comment != uuid.Nil {
			thread := messageKey(slackId, m.ThreadTs)
			if out.ParentUUID, err = importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityMessage, thread); err != nil {
				return out, fmt.Errorf("reading what this import made for message %s: %w", thread, err)
			}
		}
		if m.AuthorSlack != "" {
			authorUUID, err := importModels.LookupIdMapping(ctx, chunk.ImportId, importModels.EntityUser, m.AuthorSlack)
			if err != nil {
				return out, fmt.Errorf("reading what this import made for user %s: %w", m.AuthorSlack, err)
			}
			if authorUUID != uuid.Nil {
				out.AuthorUUID = authorUUID.String()
				// Name is not strictly needed; OpenSearch denorm
				// degrades to empty if missing.
			}
		}
	}

	return out, nil
}

// decodeIdMapMeta decodes what this import recorded with a Slack id into v;
// an id with no row, or no metadata, leaves v as it was.
func decodeIdMapMeta(ctx context.Context, importId uuid.UUID, entityType, slackId string, v any) error {
	md, err := importModels.GetIdMapMetadata(ctx, importId, entityType, slackId)
	if err != nil {
		return fmt.Errorf("reading what this import recorded with %s %s: %w", entityType, slackId, err)
	}
	if len(md) == 0 {
		return nil
	}
	if err := json.Unmarshal(md, v); err != nil {
		return fmt.Errorf("what this import recorded with %s %s: %w", entityType, slackId, err)
	}
	return nil
}

// indexFile writes an imported file's OpenSearch entry, with its parent
// post's or chat's fields.
func indexFile(ctx context.Context, p *fileParentInfo, att *dgraphStruct.DgraphAttachment) {
	switch p.Kind {
	case "post":
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
		helpers.GoSafeNamed("attachmentDomain.UpsertAttachmentInOpenSearchSafe", func() { attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAttachment) })
	case "chat":
		// Mirror native CreateChatWithAttachmentsInOpenSearch
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
		helpers.GoSafeNamed("attachmentDomain.UpsertAttachmentInOpenSearchSafe", func() { attachmentDomain.UpsertAttachmentInOpenSearchSafe(osAttachment) })
	}
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

// fileSource is where the file stage reads a file's bytes: from the export,
// when it carries the file (ParsedExport.Uploads), or from the file's link.
// An export read through Slack's API (api_reader.go) carries every file its
// reader could read, and its links open only with the token the reader held
// for the read: a file it left out isn't fetched, since its link answers
// anyone else with Slack's sign-in page.
type fileSource struct {
	arc       *Archive
	uploads   map[string]*zip.File
	linksOpen bool
}

// open is the file's bytes and the type they were served as ("" when there
// was none to go by); finished is a chunk it has already finished (a
// placeholder, or a retry), with err the chunk's answer.
func (s fileSource) open(ctx context.Context, chunk *importModels.Chunk, meta *fileChunkMeta) (io.ReadCloser, string, bool, error) {
	if zf := s.uploads[meta.FileID]; zf != nil {
		rc, err := s.arc.OpenEntry(ctx, zf)
		if err != nil {
			return nil, "", false, fmt.Errorf("open %s in the export: %w", zf.Name, err)
		}
		return rc, "", false, nil
	}
	if !s.linksOpen {
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_NOT_READ",
			fmt.Sprintf("%s wasn't read from Slack with its conversation; placeholder created", meta.Name), nil)
		return nil, "", true, filePlaceholder(ctx, chunk, meta, map[string]interface{}{"reason": "not_read"})
	}
	return fetchFileLink(ctx, chunk, meta)
}

// filePlaceholder maps the file to a placeholder, so a re-run doesn't try it
// again, and finishes its chunk with nothing imported; why says what became
// of it. A mapping that can't be saved fails the chunk rather than finishing
// it unmapped.
func filePlaceholder(ctx context.Context, chunk *importModels.Chunk, meta *fileChunkMeta, why map[string]interface{}) error {
	why["placeholder"] = true
	if err := importModels.RecordCreated(ctx, chunk.ImportId, importModels.EntityFile, meta.FileID,
		uuid.New(), nil, mustMarshal(why), ""); err != nil {
		return err
	}
	return importModels.FinishChunk(ctx, chunk.Id, 0, nil)
}

// fetchFileLink is the file at its link: the body, open, and the type it was
// served as; or a chunk finished: a placeholder for a link refused or gone, a
// retry for a rate limit.
func fetchFileLink(ctx context.Context, chunk *importModels.Chunk, meta *fileChunkMeta) (io.ReadCloser, string, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta.URL, nil)
	if err != nil {
		return nil, "", false, err
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
		return nil, "", true, filePlaceholder(ctx, chunk, meta, map[string]interface{}{"reason": "ssrf_blocked"})
	}

	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return nil, "", false, fmt.Errorf("slack file fetch: %w", err)
	}
	if resp.StatusCode < 400 {
		return resp.Body, resp.Header.Get("Content-Type"), false, nil
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		importModels.LogImportError(ctx, chunk.ImportId, &chunk.Id,
			importModels.EntityFile, meta.FileID,
			importModels.SeverityWarning, "FILE_GONE",
			fmt.Sprintf("Slack returned %d for %s; placeholder created", resp.StatusCode, meta.Name),
			mustMarshal(map[string]interface{}{"url": meta.URL}))
		// Map to a placeholder uuid so re-runs don't re-fetch.
		return nil, "", true, filePlaceholder(ctx, chunk, meta, map[string]interface{}{"status_code": resp.StatusCode})

	case http.StatusTooManyRequests:
		// Respect Retry-After if present, else default 5s.
		retryAfter := 5 * time.Second
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil && secs > 0 {
				retryAfter = time.Duration(secs) * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return nil, "", true, ctx.Err()
		case <-time.After(retryAfter):
		}
		_ = importModels.ResetChunkForRetry(ctx, chunk.Id, "rate limited; retry-after honoured")
		return nil, "", true, errReaperOnly
	}
	return nil, "", false, fmt.Errorf("slack returned %d", resp.StatusCode)
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
	// By their uuids: GetDgraphUserInfoByUUIDs takes graph uids, and refused
	// these, so every attachment got id-only participants.
	users, err := userDomain.GetActiveDgraphUsersByUUIDsLight(ctx, memberUUIDs)
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
