package business

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	attachmentModel "github.com/akashc777/OneCamp/models/postgres/Attachment"
	formModel "github.com/akashc777/OneCamp/models/postgres/Form"
	"github.com/google/uuid"
)

// Tables B5c-2 (airtable-07's branding): a form's cover and logo (look.go).

// uploads stands in for the attachments a form's pictures are, by id; it puts
// the seams back when the test ends.
func uploads(t *testing.T, files map[uuid.UUID]*attachmentModel.Attachment, logo string) {
	t.Helper()
	wasOf, wasLogo := attachmentOf, workspaceLogo
	t.Cleanup(func() { attachmentOf, workspaceLogo = wasOf, wasLogo })
	attachmentOf = func(_ context.Context, id, src string) (*attachmentModel.Attachment, error) {
		a, ok := files[uuid.MustParse(id)]
		if !ok || src != LookSource {
			return nil, sql.ErrNoRows
		}
		return a, nil
	}
	workspaceLogo = func() string { return logo }
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

func TestAFormWearsOnlyItsSaversOwnPictures(t *testing.T) {
	me, them := uuid.New(), uuid.New()
	files := map[uuid.UUID]*attachmentModel.Attachment{}
	add := func(by uuid.UUID, key string, deleted bool) uuid.UUID {
		a := &attachmentModel.Attachment{Uuid: uuid.New(), ObjKey: key, SrcKey: LookSource, CreatedBy: by}
		if deleted {
			a.DeletedAt = time.Now()
		}
		files[a.Uuid] = a
		return a.Uuid
	}
	mine, jpeg := add(me, "forms/a/cover.png", false), add(me, "forms/a/logo.JPG", false)
	theirs, pdf, svg := add(them, "forms/a/x.png", false), add(me, "forms/a/brief.pdf", false), add(me, "forms/a/logo.svg", false)
	gone := add(me, "forms/a/old.webp", true)
	uploads(t, files, "")
	for _, c := range []struct {
		name        string
		cover, logo *uuid.UUID
		was         *formModel.Form
		ok          bool
	}{
		{"the saver's own PNG and JPEG", ptr(mine), ptr(jpeg), nil, true},
		{"none", nil, nil, nil, true},
		{"someone else's upload", ptr(theirs), nil, nil, false},
		{"a PDF", nil, ptr(pdf), nil, false},
		{"an SVG, which could run", nil, ptr(svg), nil, false},
		{"a deleted upload", ptr(gone), nil, nil, false},
		{"no such upload", ptr(uuid.New()), nil, nil, false},
		{"the cover it already wears, which another manager put up", ptr(theirs), nil, &formModel.Form{CoverID: ptr(theirs)}, true},
		{"another manager's upload in place of the cover it wears", ptr(theirs), nil, &formModel.Form{CoverID: ptr(mine)}, false},
	} {
		err := checkLook(context.Background(), formModel.Form{CoverID: c.cover, LogoID: c.logo}, c.was, me)
		var fe *FormError
		if c.ok && err != nil {
			t.Errorf("%s: refused (%v), want kept", c.name, err)
		}
		if !c.ok && !errors.As(err, &fe) {
			t.Errorf("%s: %v, want refused in words", c.name, err)
		}
	}
	// The store failing is said as itself, never as the saver's mistake.
	attachmentOf = func(context.Context, string, string) (*attachmentModel.Attachment, error) {
		return nil, errors.New("down")
	}
	var fe *FormError
	if err := checkLook(context.Background(), formModel.Form{CoverID: ptr(mine)}, nil, me); err == nil || errors.As(err, &fe) {
		t.Errorf("a store that fails: %v, want its own error", err)
	}
}

func TestCheckFormReadsAPictureByItsId(t *testing.T) {
	in := Input{Title: "Sign-up", Fields: []Field{{Id: "q1", Label: "Name", Type: ShortText}}}
	f, err := CheckForm(in)
	if err != nil || f.CoverID != nil || f.LogoID != nil {
		t.Fatalf("no pictures: %v %v %v", err, f.CoverID, f.LogoID)
	}
	id := uuid.New()
	in.Cover, in.Logo = " "+id.String()+" ", ""
	if f, err = CheckForm(in); err != nil || f.CoverID == nil || *f.CoverID != id || f.LogoID != nil {
		t.Fatalf("a cover: %v %v %v", err, f.CoverID, f.LogoID)
	}
	in.Logo = "logo.png"
	var fe *FormError
	if _, err = CheckForm(in); !errors.As(err, &fe) || fe.Error() != "That logo isn't a picture added to this form." {
		t.Fatalf("a logo that isn't an id: %v", err)
	}
}

func TestALinksPageAsksForTheFormsPictures(t *testing.T) {
	cover := uuid.New()
	files := map[uuid.UUID]*attachmentModel.Attachment{cover: {Uuid: cover, ObjKey: "forms/a/cover.png", SrcKey: LookSource}}
	// A closed form: its link still serves its pictures, so its builder shows
	// them before it opens.
	form := &formModel.Form{CoverID: ptr(cover), Active: false}
	tok := "tok4tok4tok4tok4tok4tok4"
	wasByToken := formByToken
	t.Cleanup(func() { formByToken = wasByToken })
	formByToken = func(_ context.Context, token string) (*formModel.Form, error) {
		if token != tok {
			return nil, nil
		}
		return form, nil
	}
	ctx := context.Background()

	// No logo of its own and none for the workspace: the page asks for its cover only.
	uploads(t, files, "")
	if c, l := lookPaths(form, tok); c != "/public/form/"+tok+"/cover" || l != "" {
		t.Errorf("paths: %q %q", c, l)
	}
	// By its whole object name: an upload's bytes live under the bucket's prefix (MF1).
	if key, err := LookPicture(ctx, tok, "cover"); err != nil || key != attachmentBusiness.ObjectName("forms/a/cover.png") || !strings.HasSuffix(key, "/forms/a/cover.png") {
		t.Errorf("the cover: %q %v", key, err)
	}
	if _, err := LookPicture(ctx, tok, "logo"); !errors.Is(err, ErrNoPicture) {
		t.Errorf("no logo: %v, want ErrNoPicture", err)
	}
	// A token of another shape, and one of the shape that names no form.
	for _, other := range []string{"nope", "nope4nope4nope4nope4nope"} {
		if _, err := LookPicture(ctx, other, "cover"); !errors.Is(err, ErrNoForm) {
			t.Errorf("a link that names no form (%q): %v, want ErrNoForm", other, err)
		}
	}

	// The workspace's logo, for a form with none of its own.
	uploads(t, files, "systemFileUpload/email_logo_1.png")
	if _, l := lookPaths(form, tok); l != "/public/form/"+tok+"/logo" {
		t.Errorf("the workspace's logo: %q", l)
	}
	if key, err := LookPicture(ctx, tok, "logo"); err != nil || key != "systemFileUpload/email_logo_1.png" {
		t.Errorf("the workspace's logo: %q %v", key, err)
	}

	// A cover whose upload was deleted since: none, rather than a broken picture.
	files[cover].DeletedAt = time.Now()
	if _, err := LookPicture(ctx, tok, "cover"); !errors.Is(err, ErrNoPicture) {
		t.Errorf("a deleted cover: %v, want ErrNoPicture", err)
	}
}
