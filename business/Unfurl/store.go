package business

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	postgressStruct "github.com/akashc777/OneCamp/models/postgres"
	"github.com/google/uuid"
)

// What /unfurl/store keeps with a doc: a picture typed or pasted as a link
// (![](https://…)), or a bookmark's icon. Either way it is fetched once, here,
// through safefetch, and stored with the doc as an upload is: readers load it
// from this server, never from the site (N28), and a guest the doc is shared
// with can see it (B13).
const (
	KindImage = "image"
	KindIcon  = "icon"
)

const (
	// iconMaxBytes bounds an icon: a favicon is a few KB.
	iconMaxBytes = 256 << 10
	// imageMaxBytes bounds a picture whatever the workspace's upload limit.
	imageMaxBytes = 25 << 20
	// imageTimeout is how long a picture may take to arrive.
	imageTimeout = 10 * time.Second
)

var (
	// ErrBadKind is a kind other than image or icon.
	ErrBadKind = errors.New("unfurl: kind is image or icon")
	// ErrNoSuchDoc is a doc that doesn't exist, or that the person can't see.
	ErrNoSuchDoc = errors.New("unfurl: no such doc")
	// ErrCantEdit is a doc the person can read but not edit.
	ErrCantEdit = errors.New("unfurl: the doc can't be edited by this person")
)

// NotStoredError is a picture that couldn't be fetched or isn't one.
type NotStoredError struct {
	// Reason is one of the card's reasons, or "too_large" or "not_image".
	Reason string
}

func (e *NotStoredError) Error() string { return "unfurl: not stored (" + e.Reason + ")" }

// Reasons only a stored picture has.
const (
	ReasonTooLarge = "too_large"
	ReasonNotImage = "not_image"
)

// Stored is a picture kept with a doc.
type Stored struct {
	ObjectUUID  string `json:"object_uuid"`
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// storableTypes are the pictures that may be stored, as the bytes say (never
// the site's header or the link's extension), with the extension each is
// stored under. Never SVG: it can carry a script.
var storableTypes = map[string]string{
	"image/png":                ".png",
	"image/jpeg":               ".jpg",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/bmp":                ".bmp",
	"image/x-icon":             ".ico",
	"image/vnd.microsoft.icon": ".ico",
	"image/avif":               ".avif",
}

// Seams for tests.
var (
	resolveAccess = func(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
		return accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	}
	uploadToDoc = func(ctx context.Context, body []byte, name, docUUID string) (uuid.UUID, error) {
		return userBusiness.UploadUserFile(ctx, bytes.NewReader(body), int64(len(body)), name, postgressStruct.ATTACHMENT_SRC_DOC, docUUID)
	}
	uploadLimit = settingsBusiness.UploadLimitBytes
)

// StoreRemote fetches the picture at rawURL and stores it with the doc, for
// someone who can edit that doc. budget is asked before anything is fetched.
func StoreRemote(ctx context.Context, who accessBusiness.Subject, docUUID, rawURL, kind string, budget func() error) (Stored, error) {
	if kind != KindImage && kind != KindIcon {
		return Stored{}, ErrBadKind
	}
	if _, err := uuid.Parse(docUUID); err != nil {
		return Stored{}, ErrNoSuchDoc
	}
	u, err := Canonical(rawURL)
	if err != nil {
		return Stored{}, err
	}
	d, err := resolveAccess(ctx, who, docUUID)
	if errors.Is(err, docBusiness.ErrNotFound) {
		return Stored{}, ErrNoSuchDoc
	}
	if err != nil {
		return Stored{}, err
	}
	if d.Deleted || d.Level < accessBusiness.LevelView {
		return Stored{}, ErrNoSuchDoc
	}
	if d.Level < accessBusiness.LevelEdit {
		return Stored{}, ErrCantEdit
	}
	if budget != nil {
		if err := budget(); err != nil {
			return Stored{}, err
		}
	}

	max := int64(iconMaxBytes)
	if kind == KindImage {
		max = imageMaxBytes
		if limit := uploadLimit(); limit > 0 && limit < max {
			max = limit
		}
	}
	res, err := fetch(ctx, u.String(), safefetch.Options{MaxBytes: max, Timeout: imageTimeout, Accept: "image/avif,image/webp,image/png,image/*;q=0.8"})
	if err != nil {
		return Stored{}, &NotStoredError{Reason: reasonFor(ctx, err)}
	}
	if res.Status != http.StatusOK {
		return Stored{}, &NotStoredError{Reason: ReasonBadStatus}
	}
	if res.Truncated {
		return Stored{}, &NotStoredError{Reason: ReasonTooLarge}
	}
	typ := sniffImage(res.Body)
	ext, ok := storableTypes[typ]
	if !ok {
		return Stored{}, &NotStoredError{Reason: ReasonNotImage}
	}
	name := fileNameFor(res.URL.Path, kind, ext)
	obj, err := uploadToDoc(ctx, res.Body, name, docUUID)
	if err != nil {
		return Stored{}, err
	}
	return Stored{ObjectUUID: obj.String(), FileName: name, ContentType: typ, Size: len(res.Body)}, nil
}

// sniffImage is the picture's type as its bytes say. Pure.
func sniffImage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	t := http.DetectContentType(body)
	if i := strings.IndexByte(t, ';'); i >= 0 {
		t = t[:i]
	}
	if t == "application/octet-stream" && isAVIF(body) {
		return "image/avif"
	}
	return t
}

// isAVIF reads an ISO media file's brand: "ftypavif" or "ftypavis". Pure.
func isAVIF(b []byte) bool {
	return len(b) >= 12 && string(b[4:8]) == "ftyp" && (string(b[8:12]) == "avif" || string(b[8:12]) == "avis")
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// fileNameFor names a stored picture from the link's last path segment,
// trimmed to safe characters, with the extension its bytes call for. Pure.
func fileNameFor(urlPath, kind, ext string) string {
	base := strings.TrimSuffix(path.Base(urlPath), path.Ext(urlPath))
	base = strings.Trim(unsafeName.ReplaceAllString(base, "-"), "-.")
	if len(base) > 60 {
		base = base[:60]
	}
	if base == "" || base == "/" {
		base = kind
	}
	return base + ext
}
