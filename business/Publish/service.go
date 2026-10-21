package business

import (
	"context"

	"errors"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	"net/url"
	"strings"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	fileAccessBusiness "github.com/akashc777/OneCamp/business/FileAccess"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	postgresStruct "github.com/akashc777/OneCamp/models/postgres"
	attachmentModels "github.com/akashc777/OneCamp/models/postgres/Attachment"
	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

const (
	// pageWorkPerMinute is how often one page is worked out afresh in a
	// minute, whoever asks: rendered (a copy kept 30 seconds answers the rest,
	// and is never counted, so one host can't keep a page at 429 for everyone),
	// a password tried, or a copy made.
	pageWorkPerMinute = 300
	// copyFileCap is the most files a copy of a page takes with it.
	copyFileCap = 200
)

// The service's reads and writes, seams for tests.
var (
	// resolve is the one access check for anyone outside (business/Guest).
	resolve = guestBusiness.ResolveExternal
	readDoc = func(ctx context.Context, docUUID string) (*dgraphStruct.DgraphDoc, error) {
		return docBusiness.GetSystemDocByUUID(ctx, docUUID)
	}
	liveGrantsOf = func(ctx context.Context, docUUID string) ([]*guestModel.GuestGrant, error) {
		return guestModel.ListActiveForResource(ctx, guestModel.ResourceDoc, docUUID)
	}
	now = time.Now
	// pageAllowed counts one piece of a page's work against its limit.
	pageAllowed = func(ctx context.Context, grant string) bool {
		return redisStore.AllowFixedWindowOrLocal(ctx, registry.PublishPageRate, []string{grant}, pageWorkPerMinute).Allowed
	}
	// unlockAllowed counts a password's try (business/Guest's limit, the
	// same for every kind of address).
	unlockAllowed = guestBusiness.UnlockTryAllowed
	// The unlock a password gives, and whether one opens a grant now.
	mintUnlock = guestBusiness.MintUnlock
	unlocks    = guestBusiness.Unlocks
	cachedPage = func(ctx context.Context, grant string) (*cached, bool) {
		var c cached
		found, err := redisStore.GetJSON(ctx, registry.PublishPage, []string{grant}, &c)
		return &c, found && err == nil
	}
	keepPage = func(ctx context.Context, grant string, c cached) {
		_ = redisStore.SetJSON(ctx, registry.PublishPage, []string{grant}, c)
	}
	dropPage = func(ctx context.Context, grant string) {
		if err := redisStore.Delete(ctx, registry.PublishPage, []string{grant}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Publish: the copy of %s couldn't be dropped: %v", grant, err)
		}
	}
	// fileOfDoc is one of a doc's files: its attachments row names the doc.
	fileOfDoc = func(ctx context.Context, docUUID, objUUID string) (*attachmentModels.Attachment, error) {
		return fileAccessBusiness.FileOf(ctx, fileAccessBusiness.Place{Kind: postgresStruct.ATTACHMENT_SRC_DOC, ID: docUUID}, objUUID)
	}
	// embedHosts are the hosts an admin added to the sites docs can embed.
	embedHosts = func(ctx context.Context) []string {
		return policyBusiness.Policies.List(ctx, policyBusiness.EmbedAllowlist)
	}
	webBase = authService.FrontendBaseURL
	apiBase = authService.BackendBaseURL
	// A copy: a private doc of the person's, its files, and its body.
	newPrivateDoc = func(ctx context.Context, by *dgraphStruct.DgraphUser, title string) (string, error) {
		d, err := docBusiness.CreateDocInTree(ctx, by, docBusiness.NewDoc{Title: title, Space: "private"}, nil)
		if err != nil {
			return "", err
		}
		return d.Uuid, nil
	}
	addFile = func(ctx context.Context, id uuid.UUID, objKey string, by uuid.UUID, docUUID string) error {
		return attachmentBusiness.CreateAttachment(ctx, id, objKey, by, postgresStruct.ATTACHMENT_SRC_DOC, docUUID)
	}
	seedCopy = func(ctx context.Context, docUUID, body, byUUID string) error {
		_, err := docBusiness.SeedBodyLive(ctx, docUUID, body, collab.By{UserUUID: byUUID})
		return err
	}
)

// cached is a rendered page as kept, for its grant as it was: a change to the
// grant (a new address, a password, indexing, copies) misses it.
type cached struct {
	DocUUID        string    `json:"doc_uuid"`
	Version        int       `json:"version"`
	Slug           string    `json:"slug,omitempty"`
	Title          string    `json:"title"`
	HTML           string    `json:"html"`
	Indexable      bool      `json:"indexable"`
	AllowDuplicate bool      `json:"allow_duplicate"`
	UpdatedAt      time.Time `json:"updated_at"`
	Icon           string    `json:"icon,omitempty"`
	Cover          string    `json:"cover,omitempty"`
	// Embeds are the admin's embed hosts the page was drawn with: a change
	// to them draws it again (D-26).
	Embeds    string `json:"embeds,omitempty"`
	CoverURL  string `json:"cover_url,omitempty"`
	Font      string `json:"font,omitempty"`
	FullWidth bool   `json:"full_width,omitempty"`
	SmallText bool   `json:"small_text,omitempty"`
}

func (c cached) page() *Page {
	return &Page{Title: c.Title, HTML: c.HTML, Indexable: c.Indexable, AllowDuplicate: c.AllowDuplicate, UpdatedAt: c.UpdatedAt,
		Icon: c.Icon, Cover: c.Cover, CoverURL: c.CoverURL, Font: c.Font, FullWidth: c.FullWidth, SmallText: c.SmallText}
}

func slugOf(g *guestModel.GuestGrant) string {
	if g.Slug == nil {
		return ""
	}
	return *g.Slug
}

// fits is whether a kept copy is the page of g as it is now.
func (c cached) fits(g *guestModel.GuestGrant, embeds []string) bool {
	return c.DocUUID == g.ResourceID && c.Version == g.UnlockVersion && c.Slug == slugOf(g) &&
		c.Indexable == g.Indexable && c.AllowDuplicate == g.AllowCopies && c.Embeds == embedsKey(embeds)
}

// embedsKey is the admin's embed hosts as one string, for the cache. Pure.
func embedsKey(hosts []string) string {
	return strings.Join(hosts, " ")
}

func cachedOf(g *guestModel.GuestGrant, page *Page, embeds []string) cached {
	return cached{Embeds: embedsKey(embeds), DocUUID: g.ResourceID, Version: g.UnlockVersion, Slug: slugOf(g), Title: page.Title, HTML: page.HTML,
		Indexable: page.Indexable, AllowDuplicate: page.AllowDuplicate, UpdatedAt: page.UpdatedAt, Icon: page.Icon,
		Cover: page.Cover, CoverURL: page.CoverURL, Font: page.Font, FullWidth: page.FullWidth, SmallText: page.SmallText}
}

// PagePath is a page's path on this server: its kind's address
// (/public/p/<slug>) for a published one, /public/d/<token> for a link's
// token.
func PagePath(ref Ref) string {
	if ref.Slug != "" {
		return guestBusiness.KindPage.Address(ref.Slug)
	}
	return "/public/d/" + url.PathEscape(ref.Token)
}

// PageAddress is a page's full address on this server.
func PageAddress(ref Ref) string { return strings.TrimRight(apiBase(), "/") + PagePath(ref) }

// Origin is this server's origin (scheme, host and port), as a page's policy
// names it beside 'self'; "" when the server's address isn't known.
func Origin() string {
	u, err := url.Parse(apiBase())
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// WebAddress is the web app's address for a page: /p/<slug> for a published
// address, /guest/d/<token> for a link's, the one people share; with a query
// (copy, unlock) for the app's own steps.
func WebAddress(ref Ref, query string) string {
	u := strings.TrimRight(webBase(), "/")
	if ref.Slug != "" {
		u += guestBusiness.KindPage.WebAddress(ref.Slug)
	} else {
		u += "/guest/d/" + url.PathEscape(ref.Token)
	}
	if query != "" {
		u += "?" + query
	}
	return u
}

// page is the grant a reference opens as a page: a doc's view-only link,
// live; every other answer is the uniform not found, but an expired address.
func page(ctx context.Context, ref Ref) (*guestModel.GuestGrant, error) {
	g, err := resolve(ctx, ref)
	switch {
	case errors.Is(err, guestBusiness.ErrGone):
		return nil, ErrGone
	case errors.Is(err, guestBusiness.ErrGuestDisabled), errors.Is(err, guestBusiness.ErrInvalidGrant), errors.Is(err, guestBusiness.ErrNotFound):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	case !guestBusiness.KindPage.Opens(g):
		// A comment link opens the app, never a page.
		return nil, ErrNotFound
	}
	return g, nil
}

// What a page's address answers now, said without the page: for the web's
// /p/<slug>, which shows every state but a live page in its own frame.
const (
	StateLive    = guestBusiness.StateLive
	StateLocked  = guestBusiness.StateLocked
	StateGone    = guestBusiness.StateGone
	StateMissing = guestBusiness.StateMissing
)

// PageState is what an address answers now: live, locked (a password page),
// gone (expired) or missing (never published, unpublished, another kind's, or
// off). Every kind's addresses answer by the one rule (business/Guest
// StateFor), the same check as serving the page, so the two never disagree;
// nothing is rendered, and nothing more is said than the page would say.
func PageState(ctx context.Context, ref Ref) (string, error) {
	g, err := resolve(ctx, ref)
	return guestBusiness.StateFor(guestBusiness.KindPage, g, err)
}

// fileURL is a page's address for one of its doc's files: with the page's
// unlock on a password page, so the picture opens as the page did.
func fileURL(ref Ref, token string) func(string) string {
	return func(obj string) string {
		u := PagePath(ref) + "/files/" + obj
		if token != "" {
			u += "?k=" + token
		}
		return u
	}
}

// openDoc is a page's doc, while it is live.
func openDoc(ctx context.Context, g *guestModel.GuestGrant) (*dgraphStruct.DgraphDoc, error) {
	doc, err := readDoc(ctx, g.ResourceID)
	switch {
	case errors.Is(err, docBusiness.ErrNotFound), errors.Is(err, docBusiness.ErrDocDeleted):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	case doc == nil || helpers.IsSoftDeleted(doc.DeletedAt) || helpers.IsSoftDeleted(doc.ArchivedAt):
		return nil, ErrNotFound
	}
	return doc, nil
}

// PageFor is a page for anyone with its address. token is a password page's
// unlock (from its address); empty for every other page.
func PageFor(ctx context.Context, ref Ref, token string) (*Page, error) {
	g, err := page(ctx, ref)
	if err != nil {
		return nil, err
	}
	locked := g.PasswordHash != nil
	if locked && !unlocks(token, g, now()) {
		return nil, ErrLocked
	}
	// A link's token is a secret: its page is never found by search engines,
	// whatever its published address allows.
	indexable := g.Indexable && ref.Slug != ""
	// Kept by the grant, for its published address: a token's page names its
	// files by its own path, so it is rendered for each ask.
	keepable := !locked && ref.Slug != ""
	if !locked {
		token = ""
	}
	hosts := embedHosts(ctx)
	if keepable {
		if c, ok := cachedPage(ctx, g.Id.String()); ok && c.fits(g, hosts) {
			p := c.page()
			p.Indexable, p.Slug = indexable, slugOf(g)
			p.DocUUID, p.Grant = g.ResourceID, g.Id.String()
			return p, nil
		}
	}
	if !pageAllowed(ctx, g.Id.String()) {
		return nil, ErrBusy
	}
	doc, err := openDoc(ctx, g)
	if err != nil {
		return nil, err
	}
	files := fileURL(ref, token)
	p := &Page{
		Slug: slugOf(g), Title: pageTitle(doc.Title), Indexable: g.Indexable, AllowDuplicate: g.AllowCopies,
		HTML:      renderer{docUUID: g.ResourceID, fileURL: files, embedHosts: hosts}.body(doc.Body),
		UpdatedAt: stamp(doc.UpdatedAt), Token: token, DocUUID: g.ResourceID, Grant: g.Id.String(),
		Font: doc.Font, FullWidth: doc.FullWidth != nil && *doc.FullWidth, SmallText: doc.SmallText != nil && *doc.SmallText,
	}
	if docBusiness.IsEmojiIcon(doc.Icon) {
		p.Icon = doc.Icon
	}
	switch cover := doc.CoverKey; {
	case strings.HasPrefix(cover, "gallery:"):
		p.Cover = strings.TrimPrefix(cover, "gallery:")
	case cover != "":
		// A picture uploaded to this doc: "<doc>/<file>".
		if owner, file, ok := strings.Cut(cover, "/"); ok && strings.EqualFold(owner, g.ResourceID) && uuid.Validate(file) == nil {
			p.CoverURL = files(strings.ToLower(file))
		}
	}
	if keepable {
		keepPage(ctx, g.Id.String(), cachedOf(g, p, hosts))
	}
	p.Indexable = indexable
	return p, nil
}

func pageTitle(t string) string {
	if t = strings.TrimSpace(t); t != "" {
		return t
	}
	return "Untitled"
}

// Unlock is a password page's unlock for its password, tried from ip: the
// token and how long it opens the page.
func Unlock(ctx context.Context, ref Ref, password, ip string) (string, time.Duration, error) {
	g, err := page(ctx, ref)
	if err != nil {
		return "", 0, err
	}
	// The address's tries first, so one address past its tries never spends
	// the page's work for everyone.
	if !unlockAllowed(ctx, helpers.RateKeyIP(ip), g) || !pageAllowed(ctx, g.Id.String()) {
		return "", 0, ErrBusy
	}
	if !guestBusiness.PasswordOpens(g, password) {
		return "", 0, ErrWrongPassword
	}
	token, lasts := mintUnlock(g, now())
	if token == "" {
		return "", 0, errors.New("publish: the server has no signing secret")
	}
	return token, lasts, nil
}

// FileFor is one of a page's files for anyone with the page: only its own
// doc's (D-25, condition 3), only as the page itself would open.
func FileFor(ctx context.Context, ref Ref, objUUID, token string) (*File, error) {
	if uuid.Validate(objUUID) != nil {
		return nil, ErrNotFound
	}
	g, err := page(ctx, ref)
	if err != nil {
		return nil, err
	}
	locked := g.PasswordHash != nil
	if locked && !unlocks(token, g, now()) {
		return nil, ErrLocked
	}
	if _, err := openDoc(ctx, g); err != nil {
		return nil, err
	}
	att, err := fileOfDoc(ctx, g.ResourceID, strings.ToLower(objUUID))
	switch {
	case errors.Is(err, fileAccessBusiness.ErrNoSuchFile):
		return nil, ErrNotFound
	case err != nil:
		return nil, err
	}
	return &File{ObjKey: att.ObjKey, ETag: `"` + strings.ToLower(objUUID) + `"`, Indexable: g.Indexable && ref.Slug != "", Private: locked}, nil
}

// Duplicate copies a page into the person's own docs, as the page shows it: a
// private doc of theirs with its title, its body without what the page hides,
// and its pictures and files as the copy's own. token is a password page's
// unlock. The copy's id.
func Duplicate(ctx context.Context, by *dgraphStruct.DgraphUser, agentUUID string, ref Ref, token string) (string, error) {
	if by == nil {
		return "", ErrForbidden
	}
	owner, err := uuid.Parse(by.Uuid)
	if err != nil || agentUUID != "" {
		return "", ErrForbidden
	}
	g, err := page(ctx, ref)
	if err != nil {
		return "", err
	}
	if g.PasswordHash != nil && !unlocks(token, g, now()) {
		return "", ErrLocked
	}
	if !g.AllowCopies {
		return "", ErrNoCopies
	}
	if !pageAllowed(ctx, g.Id.String()) {
		return "", ErrBusy
	}
	doc, err := openDoc(ctx, g)
	if err != nil {
		return "", err
	}
	copyUUID, err := newPrivateDoc(ctx, by, strings.TrimSpace(doc.Title))
	if err != nil {
		return "", err
	}
	// The doc's files the page shows become the copy's own: a row of the
	// copy's for the same stored file, which a purge keeps while any row
	// holds it.
	base := strings.TrimRight(apiBase(), "/")
	given := map[string]string{}
	for i, file := range filesIn(g.ResourceID, doc.Body) {
		if i >= copyFileCap {
			break
		}
		att, err := fileOfDoc(ctx, g.ResourceID, file)
		if err != nil {
			continue
		}
		id := uuid.New()
		if err := addFile(ctx, id, att.ObjKey, owner, copyUUID); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Publish: a file of %s wasn't copied to %s: %v", g.ResourceID, copyUUID, err)
			continue
		}
		given[file] = base + "/doc/getDocAttachment/" + copyUUID + "/" + id.String()
	}
	body := renderer{docUUID: g.ResourceID, fileURL: func(obj string) string { return given[obj] }}.copyOf(doc.Body)
	if strings.TrimSpace(body) != "" {
		if err := seedCopy(ctx, copyUUID, body, by.Uuid); err != nil {
			return "", err
		}
	}
	return copyUUID, nil
}

// changed drops the kept copies of a doc's pages after the doc changed (a
// save, a trash, a restore): their next answer is rendered afresh.
func changed(ctx context.Context, docUUID string) {
	if _, err := uuid.Parse(docUUID); err != nil {
		return
	}
	grants, err := liveGrantsOf(ctx, docUUID)
	if err != nil {
		return
	}
	for _, g := range grants {
		if g.Slug != nil {
			dropPage(ctx, g.Id.String())
		}
	}
}
