package business

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net/url"
	"path"
	"strings"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	contentcodec "github.com/akashc777/OneCamp/business/ContentCodec"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/avscan"
	"github.com/akashc777/OneCamp/helpers/uploadsafe"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// The asset uploader: an imported page's images go to the doc's attachments,
// as an image dropped into the editor does, so no base64 and no blob: address
// ever lands in a body, and a file beside a page in a zip is not lost.

// maxImagesPerDoc bounds the uploads one page can cause.
const maxImagesPerDoc = 200

// uploadAsset stores one image as an attachment of a doc and returns the
// address the editor shows it from. Tests swap it.
var uploadAsset = func(ctx context.Context, userID uuid.UUID, docUUID, name string, r io.Reader, size int64) (objID, src string, err error) {
	safe, err := uploadsafe.PrepareUpload(r, name, uploadsafe.Options{AllowInlineImages: true})
	if err != nil {
		return "", "", err
	}
	if !strings.HasPrefix(safe.ContentType, "image/") || strings.Contains(safe.ContentType, "svg") {
		return "", "", fmt.Errorf("not an image (%s)", safe.ContentType)
	}
	if minioInit.MinioClient == nil {
		return "", "", ErrUnavailable
	}
	id := uuid.New()
	objName := fmt.Sprintf("%v/%v_%v", userID, id, safe.SafeName)
	full := "userFileUpload/" + objName
	bucket := helpers.UserUploadBucket()
	_, scan, err := attachmentBusiness.SafeUploadToMinio(ctx, safe.Reader, attachmentBusiness.SafeUploadOptions{
		Bucket: bucket, ObjectName: full, ContentType: safe.ContentType,
		ContentDisposition: safe.ContentDisposition, Size: size,
	})
	if err != nil {
		return "", "", err
	}
	if scan.Verdict == avscan.VerdictInfected {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, full, minio.RemoveObjectOptions{})
		return "", "", fmt.Errorf("blocked: it carries a virus signature (%s)", scan.Signature)
	}
	if err := attachmentBusiness.CreateAttachment(ctx, id, objName, userID, postgresStruct.ATTACHMENT_SRC_DOC, docUUID); err != nil {
		_ = minioInit.MinioClient.RemoveObject(context.Background(), bucket, full, minio.RemoveObjectOptions{})
		return "", "", err
	}
	return id.String(), authService.BackendBaseURL() + "/doc/getDocAttachment/" + docUUID + "/" + id.String(), nil
}

// uploadImages uploads a page's images, from the import (a file beside it)
// or from the body itself (a data: image), and points each at its upload. An
// image that can't be uploaded is left out, and said so. Images on the web
// keep their address.
func (m *docMaker) uploadImages(ctx context.Context, docUUID string, pg Page, body *contentcodec.Node) (*contentcodec.Node, []Skipped) {
	var lost []Skipped
	limit := settingsBusiness.UploadLimitBytes()
	count := 0
	dir := path.Dir(pg.Key)
	var keep func(n *contentcodec.Node)
	keep = func(n *contentcodec.Node) {
		kept := n.Content[:0]
		for _, c := range n.Content {
			if c.Type != "image" {
				keep(c)
				kept = append(kept, c)
				continue
			}
			src, _ := c.Attrs["src"].(string)
			label := imageLabel(pg.Key, src)
			if isWebAddress(src) {
				kept = append(kept, c)
				continue
			}
			count++
			if count > maxImagesPerDoc {
				lost = append(lost, Skipped{What: label, Why: fmt.Sprintf("a page keeps at most %d uploaded images", maxImagesPerDoc)})
				continue
			}
			r, size, name, err := m.openImage(dir, src, pg.Assets, limit)
			if err != nil {
				lost = append(lost, Skipped{What: label, Why: err.Error()})
				continue
			}
			objID, url, err := uploadAsset(ctx, m.userID, docUUID, name, r, size)
			r.Close()
			if err != nil {
				helpers.LogWarnWithContext(ctx, "content io: image %s of doc %s was not uploaded: %v", label, docUUID, err)
				lost = append(lost, Skipped{What: label, Why: "it could not be uploaded"})
				continue
			}
			c.Attrs["src"] = url
			c.Attrs["id"] = objID
			if _, ok := c.Attrs["fileName"]; !ok {
				c.Attrs["fileName"] = name
			}
			kept = append(kept, c)
		}
		n.Content = kept
	}
	keep(body)
	return contentcodec.Normalize(body, nil), lost
}

// isWebAddress is an image source on the web, which stays as it is.
func isWebAddress(src string) bool {
	low := strings.ToLower(strings.TrimSpace(src))
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "//")
}

// openImage opens an image an imported body names: a data: image, or a file
// of the import by its path relative to the page.
func (m *docMaker) openImage(dir, src string, assets AssetOpener, limit int64) (io.ReadCloser, int64, string, error) {
	if strings.HasPrefix(strings.ToLower(src), "data:") {
		data, mediaType, err := decodeDataURL(src, limit)
		if err != nil {
			return nil, 0, "", err
		}
		name := "image"
		if exts, _ := mime.ExtensionsByType(mediaType); len(exts) > 0 {
			name += exts[0]
		}
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), name, nil
	}
	if assets == nil || strings.HasPrefix(src, "/") {
		return nil, 0, "", fmt.Errorf("the image is not in the import")
	}
	p := src
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	if unescaped, err := url.PathUnescape(p); err == nil {
		p = unescaped
	}
	p = path.Clean(path.Join(dir, p))
	r, size, err := assets.Open(p)
	if err != nil {
		return nil, 0, "", fmt.Errorf("the image is not in the import")
	}
	if size > limit {
		r.Close()
		return nil, 0, "", fmt.Errorf("the image is %s; uploads are at most %s", helpers.ReadableBytes(size), helpers.ReadableBytes(limit))
	}
	return r, size, path.Base(p), nil
}

// decodeDataURL reads a base64 data: image.
func decodeDataURL(src string, limit int64) ([]byte, string, error) {
	meta, payload, ok := strings.Cut(src[len("data:"):], ",")
	if !ok || !strings.HasSuffix(strings.ToLower(meta), ";base64") {
		return nil, "", fmt.Errorf("the embedded image could not be read")
	}
	mediaType := strings.ToLower(strings.TrimSuffix(strings.ToLower(meta), ";base64"))
	if !strings.HasPrefix(mediaType, "image/") || strings.Contains(mediaType, "svg") {
		return nil, "", fmt.Errorf("the embedded file is not an image")
	}
	if int64(base64.StdEncoding.DecodedLen(len(payload))) > limit {
		return nil, "", fmt.Errorf("the embedded image is over the upload limit (%s)", helpers.ReadableBytes(limit))
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil {
		return nil, "", fmt.Errorf("the embedded image could not be read")
	}
	return data, mediaType, nil
}

// imageLabel names an image in what an import says it left out.
func imageLabel(pageKey, src string) string {
	if strings.HasPrefix(strings.ToLower(src), "data:") {
		return pageKey + ": an embedded image"
	}
	return pageKey + ": " + src
}
