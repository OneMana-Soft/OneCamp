// Package business (ProjectLink) is a project's links (basecamp-16): the
// tools its people work in elsewhere, a Figma file, a Drive folder, a GitHub
// repository, as cards on the project's home. This server reads a link's page
// once, when it is added (business/Unfurl, counted in the adder's fetches for
// the minute), and keeps what the page says and its pictures with the
// project, so the home never asks the site again and no one's browser loads a
// picture from it.
package business

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	unfurlBusiness "github.com/akashc777/OneCamp/business/Unfurl"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	linkModel "github.com/akashc777/OneCamp/models/postgres/ProjectLink"
	softDelete "github.com/akashc777/OneCamp/models/postgres/SoftDelete"
	"github.com/google/uuid"
)

// MostLinks is how many links a project's home holds.
const MostLinks = 50

// What project_links' columns hold (migration 544), in characters.
const (
	titleRunes       = 200
	descriptionRunes = 500
	siteRunes        = 120
)

// picturesWithin bounds the reads of a new link's icon and picture: a site
// slow to send them leaves the link without them, never unadded.
const picturesWithin = 10 * time.Second

var (
	// ErrInvalidURL is an address that isn't an http or https page's.
	ErrInvalidURL = unfurlBusiness.ErrInvalidURL
	// ErrDuplicate is an address one of the project's links has.
	ErrDuplicate = linkModel.ErrDuplicate
	// ErrFull is a project that holds MostLinks.
	ErrFull = linkModel.ErrFull
	// ErrNotFound is a link the project doesn't have.
	ErrNotFound = errors.New("project links: no such link")
	// ErrNoTitle is a link renamed to nothing.
	ErrNoTitle = errors.New("project links: a link needs a title")
	// ErrNoPicture is a link with no stored picture of that kind.
	ErrNoPicture = errors.New("project links: no such picture")
)

// errOverFetches is a read the adder's fetches for the minute don't cover.
var errOverFetches = errors.New("project links: over the fetches for the minute")

// View is a link as the project's home shows it.
type View struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	Host        string    `json:"host"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	SiteName    string    `json:"site_name"`
	HasIcon     bool      `json:"has_icon"`
	HasImage    bool      `json:"has_image"`
	CreatedAt   time.Time `json:"created_at"`
}

// Seams for tests.
var (
	unfurl       = unfurlBusiness.Unfurl
	fetchPicture = unfurlBusiness.FetchPicture
	allowFetch   = unfurlBusiness.AllowFetch
	keepPicture  = func(ctx context.Context, p unfurlBusiness.Picture, project uuid.UUID) (uuid.UUID, error) {
		return userBusiness.UploadUserFile(ctx, bytes.NewReader(p.Body), int64(len(p.Body)), p.Name, postgresStruct.ATTACHMENT_SRC_PROJECT_LINK, project.String())
	}
	room        = linkModel.Room
	create      = linkModel.Create
	setPictures = linkModel.SetPictures
	list        = linkModel.List
	get         = linkModel.Get
	rename      = linkModel.Rename
	remove      = linkModel.Delete
	attachment  = attachmentBusiness.GetAttachmentByObjUUID
)

// List is the project's links, in its order ([] when it has none).
func List(ctx context.Context, project uuid.UUID) ([]View, error) {
	links, err := list(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(links))
	for _, l := range links {
		out = append(out, viewOf(l))
	}
	return out, nil
}

// Add adds a link to rawURL to the project, as by, last in its order. The
// address is made canonical (tracking parameters off) and refused before its
// page is read when it isn't a web page's (ErrInvalidURL), when one of the
// project's links has it (ErrDuplicate) or when the project is full
// (ErrFull). The link is named by title, else by what its page says, else by
// its site, else by its host. A page this server can't read, or one over the
// adder's fetches for the minute, still makes a link: a plain one.
func Add(ctx context.Context, project, by uuid.UUID, rawURL, title string) (View, error) {
	u, err := unfurlBusiness.Canonical(rawURL)
	if err != nil {
		return View{}, ErrInvalidURL
	}
	address := u.String()
	if err := room(ctx, project, address, MostLinks); err != nil {
		return View{}, err
	}
	card, err := unfurl(ctx, address, budget(ctx, "link", by))
	if err != nil || !card.Fetched {
		if err != nil {
			helpers.LogInfoWithContext(ctx, "business/ProjectLink.Add: %s is kept as a plain link: %v", u.Host, err)
		}
		card = unfurlBusiness.Card{}
	}
	l := linkModel.Link{
		ProjectUUID: project,
		URL:         address,
		Description: words(card.Description, descriptionRunes),
		SiteName:    words(card.SiteName, siteRunes),
	}
	l.Title = firstWords(titleRunes, title, card.Title, card.SiteName, unfurlBusiness.SiteHost(u))
	made, err := create(ctx, l, by, MostLinks)
	if err != nil {
		return View{}, err
	}
	if card.IconURL != "" || card.ImageURL != "" {
		keepPictures(ctx, made, by, card)
	}
	return viewOf(*made), nil
}

// keepPictures keeps the icon and the picture card points at with the link,
// one after the other within picturesWithin, each counted as a store in by's
// fetches. Best effort: a picture that can't be read, isn't one or can't be
// kept leaves the link without it.
func keepPictures(ctx context.Context, l *linkModel.Link, by uuid.UUID, card unfurlBusiness.Card) {
	ctx, cancel := context.WithTimeout(ctx, picturesWithin)
	defer cancel()
	keep := func(address, kind string) *uuid.UUID {
		if address == "" {
			return nil
		}
		if err := budget(ctx, "store", by)(); err != nil {
			helpers.LogInfoWithContext(ctx, "business/ProjectLink: link %s keeps no %s: %v", l.ID, kind, err)
			return nil
		}
		p, err := fetchPicture(ctx, address, kind)
		if err != nil {
			helpers.LogInfoWithContext(ctx, "business/ProjectLink: link %s keeps no %s: %v", l.ID, kind, err)
			return nil
		}
		id, err := keepPicture(ctx, p, l.ProjectUUID)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/ProjectLink: link %s couldn't keep its %s: %v", l.ID, kind, err)
			return nil
		}
		return &id
	}
	icon := keep(card.IconURL, unfurlBusiness.KindIcon)
	image := keep(card.ImageURL, unfurlBusiness.KindImage)
	if icon == nil && image == nil {
		return
	}
	if err := setPictures(context.WithoutCancel(ctx), l.ProjectUUID, l.ID, icon, image); err != nil {
		helpers.LogErrorWithContext(ctx, "business/ProjectLink: link %s couldn't take its pictures: %v", l.ID, err)
		return
	}
	l.Icon, l.Image = icon, image
}

// budget is one fetch of action in by's fetches for the minute, as the
// unfurl previews the app asks for count theirs.
func budget(ctx context.Context, action string, by uuid.UUID) func() error {
	return func() error {
		if ok, _ := allowFetch(ctx, action, by.String()); !ok {
			return errOverFetches
		}
		return nil
	}
}

// Rename sets a link's title, folded and cut as a page's is.
func Rename(ctx context.Context, project, id uuid.UUID, title string) error {
	t := words(title, titleRunes)
	if t == "" {
		return ErrNoTitle
	}
	found, err := rename(ctx, project, id, t)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// Delete deletes a link softly, as by. Its pictures stay with it, for a
// restore (POST /restore/link/{id}).
func Delete(ctx context.Context, project, id, by uuid.UUID) error {
	found, err := remove(ctx, project, id, by)
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// Restore brings a deleted link of project back, with its pictures (POST
// /restore/link/{id}), while the project holds fewer than MostLinks, as an
// add needs (ErrFull). ErrDuplicate: a live link has its address since.
func Restore(ctx context.Context, project, id uuid.UUID) error {
	return softDelete.RestoreAs(ctx, softDelete.Link, id, ErrDuplicate, linkModel.Bound(project, MostLinks))
}

// Picture is the stored object name of a link's icon or image: only one kept
// for that link, as the project's link picture, and not deleted since.
func Picture(ctx context.Context, project, id uuid.UUID, kind string) (string, error) {
	l, err := get(ctx, project, id)
	if err != nil {
		return "", err
	}
	if l == nil {
		return "", ErrNotFound
	}
	var obj *uuid.UUID
	switch kind {
	case unfurlBusiness.KindIcon:
		obj = l.Icon
	case unfurlBusiness.KindImage:
		obj = l.Image
	}
	if obj == nil {
		return "", ErrNoPicture
	}
	a, err := attachment(ctx, obj.String(), postgresStruct.ATTACHMENT_SRC_PROJECT_LINK)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoPicture
	}
	if err != nil {
		return "", err
	}
	if a == nil || a.SrcValue != project.String() || helpers.IsSoftDeleted(&a.DeletedAt) {
		return "", ErrNoPicture
	}
	return attachmentBusiness.ObjectName(a.ObjKey), nil
}

// viewOf is l as the project's home shows it.
func viewOf(l linkModel.Link) View {
	host := ""
	if u, err := url.Parse(l.URL); err == nil {
		host = unfurlBusiness.SiteHost(u)
	}
	return View{
		ID:          l.ID.String(),
		URL:         l.URL,
		Host:        host,
		Title:       l.Title,
		Description: l.Description,
		SiteName:    l.SiteName,
		HasIcon:     l.Icon != nil,
		HasImage:    l.Image != nil,
		CreatedAt:   l.CreatedAt,
	}
}

// words is s with its whitespace folded to single spaces, cut to most
// characters.
func words(s string, most int) string {
	return strings.TrimSpace(helpers.TruncateRunes(strings.Join(strings.Fields(s), " "), most))
}

// firstWords is the first of names with words in it, folded and cut to most
// characters.
func firstWords(most int, names ...string) string {
	for _, n := range names {
		if w := words(n, most); w != "" {
			return w
		}
	}
	return ""
}
