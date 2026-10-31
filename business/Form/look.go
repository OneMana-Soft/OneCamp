package business

// Tables B5c-2 (airtable-07's branding): a form's cover and its logo. Its
// managers pick each from a picture they upload for the form through the
// app's one upload (POST /user/uploadFile with src_key "form"), and the form
// keeps that attachment (532). A visitor's page asks for them by the form's
// link, GET /public/form/{token}/cover and /logo, and a form with no logo of
// its own wears the workspace's, the one its emails carry.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	configModels "github.com/akashc777/OneCamp/models/postgres/Config"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/google/uuid"
)

// LookSource is the src_key a form's cover or logo is uploaded under: anyone
// signed in may upload one (business/FileAccess), and no file route serves it.
const LookSource = postgresStruct.ATTACHMENT_SRC_FORM

// lookKinds are the stored files a cover or a logo may be, by the extension
// the upload store gave them (it names a file by what its bytes are): kinds
// every browser draws, never one that could run, as an SVG could.
var lookKinds = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// ErrNoPicture is a form with no cover, or with no logo of its own and a
// workspace with none either.
var ErrNoPicture = errors.New("no such picture")

// Seams for tests: an attachment by its id and src_key, and the stored key of
// the workspace's logo ("" for none).
var (
	attachmentOf  = attachmentBusiness.GetAttachmentByObjUUID
	workspaceLogo = configModels.EmailLogo
)

// parseLook reads the attachment id a form is saved with as its cover or its
// logo: nil for "".
func parseLook(s, what string) (*uuid.UUID, error) {
	if s = strings.TrimSpace(s); s == "" {
		return nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return nil, &FormError{fmt.Sprintf("That %s isn't a picture added to this form.", what)}
	}
	return &id, nil
}

// checkLook says whether a form may wear the cover and the logo it is saved
// with: each the one it already wears (was, nil for a new form), or a picture
// the one saving it uploaded for a form, of a kind every browser draws. A
// manager can't put up someone else's upload, or a file that isn't a picture.
func checkLook(ctx context.Context, f formModel.Form, was *formModel.Form, by uuid.UUID) error {
	var hadCover, hadLogo *uuid.UUID
	if was != nil {
		hadCover, hadLogo = was.CoverID, was.LogoID
	}
	if err := checkPicture(ctx, f.CoverID, hadCover, by, "cover"); err != nil {
		return err
	}
	return checkPicture(ctx, f.LogoID, hadLogo, by, "logo")
}

// checkPicture is checkLook's word on one picture: id, the one the form
// already wears (had), or by's own upload of a picture.
func checkPicture(ctx context.Context, id, had *uuid.UUID, by uuid.UUID, what string) error {
	if id == nil || (had != nil && *had == *id) {
		return nil
	}
	a, err := attachmentOf(ctx, id.String(), LookSource)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil || a == nil || a.CreatedBy != by || !a.DeletedAt.IsZero() || !isPicture(a.ObjKey) {
		return &FormError{fmt.Sprintf("Add a PNG, JPEG, WebP or GIF picture as the form's %s.", what)}
	}
	return nil
}

// isPicture says whether a stored file is a kind of picture a cover or a
// logo may be. Pure.
func isPicture(key string) bool {
	return slices.Contains(lookKinds, strings.ToLower(path.Ext(key)))
}

// lookPaths are where a visitor's page asks for a form's cover and logo, on
// this server: "" for one it hasn't (a logo falls back to the workspace's).
func lookPaths(f *formModel.Form, token string) (cover, logo string) {
	at := "/public/form/" + token
	if f.CoverID != nil {
		cover = at + "/cover"
	}
	if f.LogoID != nil || workspaceLogo() != "" {
		logo = at + "/logo"
	}
	return cover, logo
}

// LookPicture is the stored file a visitor's page shows as a form's cover or
// logo ("cover" or "logo"), by its object name in the upload bucket: the
// form's own (an upload, under the bucket's prefix: attachment ObjectName), or
// for a logo the workspace's (its config holds the whole name).
// The form's link serves them whether the form is open or not, so its
// builder shows them before it opens: they were put up to be seen by
// whoever holds the link. ErrNoForm when the link names no form,
// ErrNoPicture when it has none.
func LookPicture(ctx context.Context, token, kind string) (string, error) {
	f, err := byLink(ctx, token)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", ErrNoForm
	}
	id := f.CoverID
	if kind == "logo" {
		id = f.LogoID
	}
	if id != nil {
		a, err := attachmentOf(ctx, id.String(), LookSource)
		switch {
		case err == nil && a != nil && a.DeletedAt.IsZero() && isPicture(a.ObjKey):
			return attachmentBusiness.ObjectName(a.ObjKey), nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return "", err
		}
	}
	if kind == "logo" {
		if key := workspaceLogo(); key != "" {
			return key, nil
		}
	}
	return "", ErrNoPicture
}
