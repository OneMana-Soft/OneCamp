package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	unfurlBusiness "github.com/akashc777/OneCamp/business/Unfurl"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	linkModel "github.com/akashc777/OneCamp/models/postgres/ProjectLink"
	"github.com/google/uuid"
)

// fakes is one test's seams: what the page says, which pictures arrive, and
// what was kept and made.
type fakes struct {
	card       unfurlBusiness.Card
	unfurlErr  error
	pictures   map[string]error // by the picture's address: nil arrives
	allowed    map[string]bool  // by budget action; missing is allowed
	roomErr    error
	unfurled   int
	fetched    []string
	budgets    []string
	kept       int
	made       *linkModel.Link
	icon, pict *uuid.UUID
}

func (f *fakes) install(t *testing.T) {
	t.Helper()
	old := []any{unfurl, fetchPicture, allowFetch, keepPicture, room, create, setPictures}
	t.Cleanup(func() {
		unfurl = old[0].(func(context.Context, string, func() error) (unfurlBusiness.Card, error))
		fetchPicture = old[1].(func(context.Context, string, string) (unfurlBusiness.Picture, error))
		allowFetch = old[2].(func(context.Context, string, string) (bool, int))
		keepPicture = old[3].(func(context.Context, unfurlBusiness.Picture, uuid.UUID) (uuid.UUID, error))
		room = old[4].(func(context.Context, uuid.UUID, string, int) error)
		create = old[5].(func(context.Context, linkModel.Link, uuid.UUID, int) (*linkModel.Link, error))
		setPictures = old[6].(func(context.Context, uuid.UUID, uuid.UUID, *uuid.UUID, *uuid.UUID) error)
	})
	unfurl = func(_ context.Context, raw string, budget func() error) (unfurlBusiness.Card, error) {
		f.unfurled++
		if budget != nil {
			if err := budget(); err != nil {
				return unfurlBusiness.Card{}, err
			}
		}
		c := f.card
		c.URL = raw
		return c, f.unfurlErr
	}
	fetchPicture = func(_ context.Context, raw, kind string) (unfurlBusiness.Picture, error) {
		f.fetched = append(f.fetched, kind+" "+raw)
		if err := f.pictures[raw]; err != nil {
			return unfurlBusiness.Picture{}, err
		}
		return unfurlBusiness.Picture{Body: []byte("png"), ContentType: "image/png", Name: kind + ".png"}, nil
	}
	allowFetch = func(_ context.Context, action, _ string) (bool, int) {
		f.budgets = append(f.budgets, action)
		ok, said := f.allowed[action]
		return ok || !said, 30
	}
	keepPicture = func(context.Context, unfurlBusiness.Picture, uuid.UUID) (uuid.UUID, error) {
		f.kept++
		return uuid.New(), nil
	}
	room = func(context.Context, uuid.UUID, string, int) error { return f.roomErr }
	create = func(_ context.Context, l linkModel.Link, _ uuid.UUID, most int) (*linkModel.Link, error) {
		if most != MostLinks {
			t.Errorf("create's cap: %d, want MostLinks", most)
		}
		l.ID, l.CreatedAt = uuid.New(), time.Now()
		f.made = &l
		return &l, nil
	}
	setPictures = func(_ context.Context, _, _ uuid.UUID, icon, image *uuid.UUID) error {
		f.icon, f.pict = icon, image
		return nil
	}
}

func add(t *testing.T, raw, title string) (View, error) {
	t.Helper()
	return Add(context.Background(), uuid.New(), uuid.New(), raw, title)
}

// A page that answers names its link: its title, its few words, its site, and
// its icon and picture, kept here. Whitespace the page sends is folded.
func TestAddNamesALinkByWhatItsPageSays(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "  Q4   launch \n designs ", SiteName: "Figma",
		Description: "The launch's screens.", IconURL: "https://figma.example/favicon.png", ImageURL: "https://figma.example/og.png"}}
	f.install(t)
	v, err := add(t, "https://www.figma.example/file/abc?utm_source=chat", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if v.Title != "Q4 launch designs" || v.SiteName != "Figma" || v.Description != "The launch's screens." {
		t.Fatalf("the card: %+v", v)
	}
	if v.URL != "https://www.figma.example/file/abc" || v.Host != "figma.example" {
		t.Fatalf("the address %q and host %q: want the canonical address, without tracking, and the host without www", v.URL, v.Host)
	}
	if !v.HasIcon || !v.HasImage || f.kept != 2 || f.icon == nil || f.pict == nil {
		t.Fatalf("pictures: icon %v image %v, kept %d, set %v %v; want both kept and set", v.HasIcon, v.HasImage, f.kept, f.icon, f.pict)
	}
	if strings.Join(f.budgets, ",") != "link,store,store" {
		t.Fatalf("budgets asked: %v, want the page as a link and each picture as a store", f.budgets)
	}
}

// The person's own title wins over the page's.
func TestAddKeepsThePersonsOwnTitle(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "Untitled - Figma"}}
	f.install(t)
	v, err := add(t, "https://figma.example/file/abc", "  Launch   screens ")
	if err != nil || v.Title != "Launch screens" || f.made == nil || f.made.Title != "Launch screens" {
		t.Fatalf("Add: %v, %+v", err, v)
	}
}

// A page with no title is named by its site, and one with neither by its host.
func TestAddNamesAPageWithNoTitleByItsSiteOrHost(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, SiteName: "GitHub"}}
	f.install(t)
	if v, err := add(t, "https://github.example/acme/app", ""); err != nil || v.Title != "GitHub" {
		t.Fatalf("by its site: %v, %+v", err, v)
	}
	f.card = unfurlBusiness.Card{Fetched: true}
	if v, err := add(t, "https://www.drive.example/folders/1", ""); err != nil || v.Title != "drive.example" {
		t.Fatalf("by its host: %v, %+v", err, v)
	}
}

// A page this server couldn't read keeps a plain link, named by its host:
// nothing else is fetched for it.
func TestAddKeepsAPlainLinkToAPageItCouldntRead(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: false, Reason: unfurlBusiness.ReasonUnreachable, IconURL: "https://x.example/i.png"}}
	f.install(t)
	v, err := add(t, "https://intranet.example/wiki", "")
	if err != nil || v.Title != "intranet.example" || v.Description != "" || v.HasIcon || len(f.fetched) != 0 {
		t.Fatalf("Add: %v, %+v, fetched %v", err, v, f.fetched)
	}
}

// Over the person's fetches for the minute, the link is still added, plain.
func TestAddOverTheFetchBudgetStillAddsAPlainLink(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "Never read"}, allowed: map[string]bool{"link": false}}
	f.install(t)
	v, err := add(t, "https://figma.example/file/abc", "")
	if err != nil || v.Title != "figma.example" || len(f.fetched) != 0 {
		t.Fatalf("Add: %v, %+v, fetched %v", err, v, f.fetched)
	}
}

// What a page says is cut to what its columns hold, by characters.
func TestAddCutsWhatAPageSaysToItsColumns(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: strings.Repeat("é", 300), Description: strings.Repeat("d", 900), SiteName: strings.Repeat("s", 400)}}
	f.install(t)
	v, err := add(t, "https://long.example/", "")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if n := len([]rune(v.Title)); n != 200 || len([]rune(v.Description)) != 500 || len([]rune(v.SiteName)) != 120 {
		t.Fatalf("lengths: title %d, description %d, site %d; want 200, 500, 120", n, len([]rune(v.Description)), len([]rune(v.SiteName)))
	}
}

// A picture that can't be read, or isn't one, leaves the link without it.
func TestAddKeepsALinkWhosePictureFails(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "Board", IconURL: "https://miro.example/i.ico", ImageURL: "https://miro.example/og.png"},
		pictures: map[string]error{"https://miro.example/i.ico": &unfurlBusiness.NotStoredError{Reason: unfurlBusiness.ReasonNotImage}}}
	f.install(t)
	v, err := add(t, "https://miro.example/board/1", "")
	if err != nil || v.HasIcon || !v.HasImage || f.kept != 1 || f.icon != nil || f.pict == nil {
		t.Fatalf("Add: %v, %+v, kept %d", err, v, f.kept)
	}
}

// An address that isn't a web page's, or that one of the project's links has,
// or a project that is full, is refused before its page is read.
func TestAddRefusesBeforeReadingThePage(t *testing.T) {
	f := &fakes{card: unfurlBusiness.Card{Fetched: true, Title: "x"}}
	f.install(t)
	for _, raw := range []string{"javascript:alert(1)", "ftp://files.example/a", "not a link", ""} {
		if _, err := add(t, raw, ""); !errors.Is(err, ErrInvalidURL) {
			t.Fatalf("%q: %v, want ErrInvalidURL", raw, err)
		}
	}
	for _, want := range []error{ErrDuplicate, ErrFull} {
		f.roomErr = want
		if _, err := add(t, "https://figma.example/file/abc", ""); !errors.Is(err, want) {
			t.Fatalf("room %v: %v", want, err)
		}
	}
	if f.unfurled != 0 || f.made != nil {
		t.Fatalf("read %d pages and made %v for refused adds", f.unfurled, f.made)
	}
}

// A rename needs words; they are folded and cut as a page's title is.
func TestRenameNeedsATitle(t *testing.T) {
	old := rename
	t.Cleanup(func() { rename = old })
	var got string
	rename = func(_ context.Context, _, _ uuid.UUID, title string) (bool, error) { got = title; return true, nil }
	if err := Rename(context.Background(), uuid.New(), uuid.New(), " \n "); !errors.Is(err, ErrNoTitle) {
		t.Fatalf("a blank title: %v", err)
	}
	if err := Rename(context.Background(), uuid.New(), uuid.New(), "  Launch \t board "); err != nil || got != "Launch board" {
		t.Fatalf("Rename: %v, %q", err, got)
	}
	rename = func(context.Context, uuid.UUID, uuid.UUID, string) (bool, error) { return false, nil }
	if err := Rename(context.Background(), uuid.New(), uuid.New(), "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a link the project doesn't have: %v", err)
	}
}

// A link's picture is answered only when it was kept for that link, as a
// project link's: anything else is ErrNoPicture.
func TestPictureAnswersOnlyALinksOwnStoredPicture(t *testing.T) {
	oldGet, oldAtt := get, attachment
	t.Cleanup(func() { get, attachment = oldGet, oldAtt })
	project, icon := uuid.New(), uuid.New()
	get = func(_ context.Context, p, id uuid.UUID) (*linkModel.Link, error) {
		if p != project {
			return nil, nil
		}
		return &linkModel.Link{ID: id, ProjectUUID: p, Icon: &icon}, nil
	}
	att := &attachmentModels.Attachment{ObjKey: "links/icon.png", SrcKey: postgresStruct.ATTACHMENT_SRC_PROJECT_LINK, SrcValue: project.String()}
	attachment = func(_ context.Context, obj, src string) (*attachmentModels.Attachment, error) {
		if obj != icon.String() || src != postgresStruct.ATTACHMENT_SRC_PROJECT_LINK {
			return nil, nil
		}
		return att, nil
	}
	ctx := context.Background()
	if key, err := Picture(ctx, project, uuid.New(), "icon"); err != nil || key != attachmentBusiness.ObjectName("links/icon.png") {
		t.Fatalf("the icon: %q %v", key, err)
	}
	for _, kind := range []string{"image", "banner", ""} {
		if _, err := Picture(ctx, project, uuid.New(), kind); !errors.Is(err, ErrNoPicture) {
			t.Fatalf("%q: %v, want ErrNoPicture", kind, err)
		}
	}
	if _, err := Picture(ctx, uuid.New(), uuid.New(), "icon"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another project's link: %v, want ErrNotFound", err)
	}
	att.SrcValue = uuid.NewString()
	if _, err := Picture(ctx, project, uuid.New(), "icon"); !errors.Is(err, ErrNoPicture) {
		t.Fatalf("a picture kept for another project: %v, want ErrNoPicture", err)
	}
	att.SrcValue, att.DeletedAt = project.String(), time.Now()
	if _, err := Picture(ctx, project, uuid.New(), "icon"); !errors.Is(err, ErrNoPicture) {
		t.Fatalf("a deleted picture: %v, want ErrNoPicture", err)
	}
}
