package business

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// uploadSlackAvatar fetches a Slack-hosted avatar URL and re-uploads it
// to our own MinIO bucket so the imported user keeps a working profile
// picture after Slack's CDN URL expires (their `image_*` URLs typically
// stay alive ~90 days post-export).
//
// On success it returns the attachment UUID — that's the value we then
// store as DgraphUser.ProfileKey, mirroring the contract used by the
// native profile-pic upload pathway:
//
//	attachments row    : id=<attUUID>, src_key="public", obj_key="<owner>/<attUUID>_<file>"
//	DgraphUser.ProfileKey = "<attUUID>"  (string)
//	GetSignedProfileURL  : looks up the attachment row by id+src_key and
//	                       presigns obj_key.
//
// Empty avatarURL or any non-recoverable failure returns "", err — the
// caller falls back to creating the user without a profile picture so
// avatar fetch flakiness never blocks the import. The error is logged
// and surfaced via importModels.LogImportError at WARNING severity.
//
// The owner UUID is the importing admin: the attachment is logically
// "uploaded" by them on behalf of the imported user, same way native
// profile uploads attribute to the uploader.
func uploadSlackAvatar(ctx context.Context, importId uuid.UUID,
	ownerUUID uuid.UUID, slackUserId, avatarURL string) (string, error) {

	if avatarURL == "" {
		return "", nil
	}

	// Pre-flight SSRF guard. The transport-level dialer also rejects
	// disallowed IPs, but ValidateOutboundURL refuses HTTP-only URLs
	// and bare-IP URLs early so we don't burn a connection.
	if _, err := helpers.ValidateOutboundURL(avatarURL, false); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport avatar URL blocked by SSRF guard slackId=%s: %+v",
			slackUserId, err)
		return "", nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, avatarURL, nil)
	if err != nil {
		return "", fmt.Errorf("avatar request build: %w", err)
	}
	resp, err := slackHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("avatar fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
		// Avatar URL already expired. Not fatal.
		helpers.LogWarnWithContext(ctx,
			"SlackImport avatar gone slackId=%s status=%d url=%s",
			slackUserId, resp.StatusCode, avatarURL)
		return "", nil
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("avatar fetch returned %d", resp.StatusCode)
	}

	// Cap avatar size at 8 MB. Slack's image_512 is normally <100 KB but
	// the original size variant can be huge. Limit reader bails mid-stream.
	const maxAvatarBytes int64 = 8 << 20
	body := io.LimitReader(resp.Body, maxAvatarBytes+1)

	rawContentType := resp.Header.Get("Content-Type")
	ext := guessAvatarExt(avatarURL, rawContentType)
	fileName := fmt.Sprintf("slack-avatar-%s%s", strings.ToLower(slackUserId), ext)
	if rawContentType == "" {
		if c := mime.TypeByExtension(ext); c != "" {
			rawContentType = c
		} else {
			rawContentType = "application/octet-stream"
		}
	}
	// Coerce avatar Content-Type. Profile pictures must be one of a
	// strict set of image types — anything else is dropped to
	// application/octet-stream to defeat XSS via avatar-served HTML/SVG.
	contentType := uploadsafe.SafeContentType(rawContentType, ext)
	disposition := uploadsafe.BuildDisposition(fileName)

	bucketName := helpers.UserUploadBucket()
	attachmentUUID := uuid.New()

	// Match UploadUserFile's key shape exactly so GetFileURLByObjectName
	// (which prepends "userFileUpload/" to attachments.obj_key) presigns
	// the correct path.
	objKey := fmt.Sprintf("%s/%s_%s", ownerUUID, attachmentUUID, fileName)
	fullObjName := path.Join("userFileUpload", objKey)

	uploadInfo, scanResult, err := attachmentBusiness.SafeUploadToMinio(ctx, body,
		attachmentBusiness.SafeUploadOptions{
			Bucket:             bucketName,
			ObjectName:         fullObjName,
			ContentType:        contentType,
			ContentDisposition: disposition,
			Size:               -1,
		})
	if err != nil {
		return "", fmt.Errorf("avatar minio put: %w", err)
	}
	if scanResult.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		helpers.LogWarnWithContext(ctx,
			"SlackImport avatar dropped: AV detected %s in %s",
			scanResult.Signature, fileName)
		return "", nil
	}
	if uploadInfo.Size > maxAvatarBytes {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		return "", fmt.Errorf("avatar exceeded %d bytes", maxAvatarBytes)
	}

	// Public src so GetSignedProfileURL accepts it; src_value is conventionally
	// empty for profile pics (the existing UploadUserFile path passes whatever
	// the client supplied; callers of GetAttachmentByObjUUID match only on id
	// + src_key).
	if err := attachmentBusiness.CreateAttachment(ctx, attachmentUUID, objKey, ownerUUID,
		postgressStruct.ATTACHMENT_SRC_PUBLIC, ""); err != nil {
		// Roll back the MinIO upload so a re-run isn't fighting an orphan.
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucketName, fullObjName,
			minio.RemoveObjectOptions{})
		return "", fmt.Errorf("avatar attachment row: %w", err)
	}

	return attachmentUUID.String(), nil
}

// guessAvatarExt returns the best file extension for the avatar payload.
// Tries the URL path first, then falls back to MIME type, then to .jpg.
// Slack's CDN routinely serves .jpg without an extension in the path so
// the fallback chain matters.
func guessAvatarExt(rawURL, contentType string) string {
	if u := strings.SplitN(rawURL, "?", 2)[0]; u != "" {
		if e := strings.ToLower(filepath.Ext(u)); e == ".png" || e == ".jpg" ||
			e == ".jpeg" || e == ".gif" || e == ".webp" {
			return e
		}
	}
	switch contentType {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".jpg"
}
