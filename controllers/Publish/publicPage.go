package controllers

// A published page's public routes (D-25): the page itself, a password's
// unlock, and the page's own files. Anyone with the address reaches them, so
// each answer is built to be the same for everyone and kept by no one longer
// than the page is published:
//
//   - The page is a complete, script-free document under a policy that runs
//     nothing and loads nothing but this server's pictures (sandbox, without
//     allow-same-origin, so it has an origin of its own: "null").
//   - No cookie is read and none is set: the browser sends this server's
//     cookies with the request, and they are ignored.
//   - A page may be kept only if the cache asks first (no-cache, an ETag), so
//     an unpublish ends it everywhere at the next request; a password page
//     isn't kept at all.
//   - Each address is counted under helpers.ClientIP, the trusted-proxy rule
//     every public route uses: behind Cloudflare and Traefik, the visitor.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	guestBusiness "github.com/akashc777/OneCamp/business/Guest"
	business "github.com/akashc777/OneCamp/business/Publish"
	viewBusiness "github.com/akashc777/OneCamp/business/ResourceView"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/akashc777/OneCamp/middleware"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
	"github.com/go-chi/chi/v5"
	"github.com/minio/minio-go/v7"
)

// Asks from one address in a minute (an IPv6 address by its /64). A page is a
// person reading, and its pictures come with it; more is a script.
const (
	pageAsksPerAddress   = 120
	fileAsksPerAddress   = 1200
	unlockAsksPerAddress = 30
)

// What a page's address counts asks under: the page kind's own surfaces, so
// another kind's address (a room's) counts apart.
var (
	pageSurface   = guestBusiness.KindPage.Surface("")
	unlockSurface = guestBusiness.KindPage.Surface("unlock")
	fileSurface   = guestBusiness.KindPage.Surface("file")
)

// rasterTypes are the pictures a page shows as they are; every other file is
// a download, never drawn by the browser.
var rasterTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/avif": true,
}

// storedFile is a file as storage hands it out.
type storedFile struct {
	Body        io.ReadSeekCloser
	ContentType string
	Modified    time.Time
}

// errNoStoredFile is a file storage doesn't have.
var errNoStoredFile = errors.New("publish: the file isn't in storage")

// Seams for tests: the service, storage, the address rule and its counter.
var (
	pageFor    = business.PageFor
	countVisit = viewBusiness.CountPageVisit
	pageState  = business.PageState
	unlockPage = business.Unlock
	fileFor    = business.FileFor
	openStored = func(ctx context.Context, objKey string) (*storedFile, error) {
		if minioInit.MinioClient == nil {
			return nil, errors.New("publish: storage isn't connected")
		}
		obj, err := minioInit.MinioClient.GetObject(ctx, helpers.UserUploadBucket(), attachmentBusiness.ObjectName(objKey), minio.GetObjectOptions{})
		if err != nil {
			return nil, err
		}
		st, err := obj.Stat()
		if err != nil {
			_ = obj.Close()
			if resp := minio.ToErrorResponse(err); resp.Code == "NoSuchKey" || resp.StatusCode == http.StatusNotFound {
				return nil, errNoStoredFile
			}
			return nil, err
		}
		return &storedFile{Body: obj, ContentType: st.ContentType, Modified: st.LastModified}, nil
	}
	clientIP = helpers.ClientIP
	// addressAllowed is every public read's one per-address limit, counted by
	// the page kind's surfaces (business/Guest Kind.Surface).
	addressAllowed = redisStore.AllowPerAddress
)

// PagePolicy is the page's Content-Security-Policy: it runs nothing, posts
// nowhere, sits in no frame, and loads only this server's pictures and its
// own style. allow-downloads lets a file link save the file.
func PagePolicy() string {
	img := "'self'"
	if o := business.Origin(); o != "" {
		img += " " + o
	}
	return "sandbox allow-downloads; default-src 'none'; img-src " + img + "; style-src " + business.StyleSource +
		"; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
}

// filePolicy is a file's own: it runs nothing, wherever it is opened.
const filePolicy = "sandbox; default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; frame-ancestors 'none'"

// common are the headers every public answer carries.
func common(h http.Header, indexable bool) {
	h.Del("Set-Cookie")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if !indexable {
		h.Set("X-Robots-Tag", "noindex, nofollow")
	}
}

// etagMatches is whether an If-None-Match names etag.
func etagMatches(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}

// answer writes one of the page's documents.
func answer(w http.ResponseWriter, r *http.Request, d business.Document) {
	h := w.Header()
	common(h, d.Indexable)
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", PagePolicy())
	switch {
	case d.Private:
		h.Set("Cache-Control", "private, no-store")
	case d.Revalidate:
		h.Set("Cache-Control", "no-cache")
		if d.ETag != "" {
			h.Set("ETag", d.ETag)
			if etagMatches(r.Header.Get("If-None-Match"), d.ETag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
	default:
		h.Set("Cache-Control", "no-store")
	}
	if d.Status == http.StatusTooManyRequests {
		h.Set("Retry-After", "60")
	}
	h.Set("Content-Length", strconv.Itoa(len(d.Body)))
	w.WriteHeader(d.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(d.Body)
	}
}

// bySlug and byToken are how a request names its page: a published address,
// or a link's token.
func bySlug(r *http.Request) business.Ref  { return business.Ref{Slug: chi.URLParam(r, "slug")} }
func byToken(r *http.Request) business.Ref { return business.Ref{Token: chi.URLParam(r, "token")} }

// PublishedPage GET /public/p/{slug}: a published page, for anyone with its
// address; ?k= is a password page's unlock.
func PublishedPage(w http.ResponseWriter, r *http.Request) { servePage(w, r, bySlug(r)) }

// LinkPage GET /public/d/{token}: a doc's view-only link, by its token, as the
// same page (one renderer for every read-only view outside the workspace).
func LinkPage(w http.ResponseWriter, r *http.Request) { servePage(w, r, byToken(r)) }

func servePage(w http.ResponseWriter, r *http.Request, ref business.Ref) {
	ctx := r.Context()
	if !addressAllowed(ctx, pageSurface, clientIP(r), pageAsksPerAddress).Allowed {
		answer(w, r, business.BusyDocument())
		return
	}
	page, err := pageFor(ctx, ref, r.URL.Query().Get("k"))
	switch {
	case err == nil:
		// The page links to itself to reload it: with its unlock on a password
		// page, so the reload opens it as this did.
		self := business.PagePath(ref)
		if page.Token != "" {
			self += "?k=" + url.QueryEscape(page.Token)
		}
		// A published address is the page's canonical one; a token's page
		// names none (a token is a secret, never to be found).
		canonical := ""
		if ref.Slug != "" {
			canonical = business.PageAddress(ref)
		}
		// Copies are a published page's option: copied by its address, with
		// the unlock (made for the link, whichever way it was reached).
		duplicate := ""
		if page.AllowDuplicate && page.Slug != "" {
			q := "copy=1"
			if page.Token != "" {
				q += "&k=" + url.QueryEscape(page.Token)
			}
			duplicate = business.WebAddress(business.Ref{Slug: page.Slug}, q)
		}
		answer(w, r, business.PageDocument(page, canonical, duplicate, self, time.Now()))
		// Counted per link, by day, under the visitors' rules (C6-3): a
		// failure to count never touches the page.
		_ = countVisit(ctx, viewBusiness.ResourceDoc, page.DocUUID, page.Grant, visitOf(r))
	case errors.Is(err, business.ErrLocked):
		answer(w, r, business.LockedDocument(business.WebAddress(ref, "unlock=1")))
	case errors.Is(err, business.ErrGone):
		answer(w, r, business.GoneDocument())
	case errors.Is(err, business.ErrBusy):
		answer(w, r, business.BusyDocument())
	case errors.Is(err, business.ErrNotFound):
		answer(w, r, business.MissingDocument())
	default:
		helpers.LogErrorWithContext(ctx, "controllers/Publish/servePage: %+v", err)
		answer(w, r, business.UnavailableDocument())
	}
}

// plain answers a public route that isn't the page with a short line.
func plain(w http.ResponseWriter, status int, line string) {
	h := w.Header()
	common(h, false)
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Security-Policy", filePolicy)
	h.Set("Cache-Control", "no-store")
	if status == http.StatusTooManyRequests {
		h.Set("Retry-After", "60")
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, line)
}

// busyAnswer is the per-address limit's 429, said as every public read's is
// (middleware.WriteRateLimited: its real wait), with an unlock answer's headers.
func busyAnswer(w http.ResponseWriter, res redisStore.RateLimitResult, msg string) {
	h := w.Header()
	common(h, false)
	h.Set("Cache-Control", "no-store")
	middleware.WriteRateLimited(w, res, registry.PublicReadRate, msg)
}

func unlockAnswer(w http.ResponseWriter, status int, body helpers.Envolope) {
	h := w.Header()
	common(h, false)
	h.Set("Cache-Control", "no-store")
	if status == http.StatusTooManyRequests {
		h.Set("Retry-After", "60")
	}
	helpers.WriteJSON(w, status, body)
}

// PublishedPageUnlock POST /public/p/{slug}/unlock: a password page's
// unlock for its password, for the web app's /p/<slug> to open the page with.
func PublishedPageUnlock(w http.ResponseWriter, r *http.Request) { unlockPageAt(w, r, bySlug(r)) }

// PublishedPageState GET /public/p/{slug}/state: what the address answers
// now ({state: live | locked | gone | missing}), for the web's /p/<slug>,
// which shows every state but a live page in its own frame. Counted with the
// page's own asks from the address, and saying nothing the page wouldn't.
func PublishedPageState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if res := addressAllowed(ctx, pageSurface, clientIP(r), pageAsksPerAddress); !res.Allowed {
		busyAnswer(w, res, "This page is busy. Try again in a moment.")
		return
	}
	state, err := pageState(ctx, bySlug(r))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Publish/PublishedPageState: %+v", err)
		unlockAnswer(w, http.StatusInternalServerError, helpers.Envolope{"msg": "This page couldn't be shown. Try again in a moment."})
		return
	}
	unlockAnswer(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{"state": state}})
}

// LinkPageUnlock POST /public/d/{token}/unlock: the same, for a link's page.
func LinkPageUnlock(w http.ResponseWriter, r *http.Request) { unlockPageAt(w, r, byToken(r)) }

func unlockPageAt(w http.ResponseWriter, r *http.Request, ref business.Ref) {
	ctx := r.Context()
	ip := clientIP(r)
	if res := addressAllowed(ctx, unlockSurface, ip, unlockAsksPerAddress); !res.Allowed {
		busyAnswer(w, res, "Too many tries from here. Wait a few minutes and try again.")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&in); err != nil {
		unlockAnswer(w, http.StatusBadRequest, helpers.Envolope{"msg": "That couldn't be read."})
		return
	}
	token, lasts, err := unlockPage(ctx, ref, in.Password, ip)
	switch {
	case err == nil:
		unlockAnswer(w, http.StatusOK, helpers.Envolope{"data": helpers.Envolope{
			"token": token, "expires_in": int(lasts.Seconds()),
			"url": business.PageAddress(ref) + "?k=" + url.QueryEscape(token),
		}})
	case errors.Is(err, business.ErrWrongPassword):
		unlockAnswer(w, http.StatusUnauthorized, helpers.Envolope{"msg": "That password doesn't open this page.", "status": "wrong_password"})
	case errors.Is(err, business.ErrBusy):
		unlockAnswer(w, http.StatusTooManyRequests, helpers.Envolope{"msg": "Too many tries. Wait a few minutes and try again.", "status": "rate_limited"})
	case errors.Is(err, business.ErrGone):
		unlockAnswer(w, http.StatusGone, helpers.Envolope{"msg": "This page has expired."})
	case errors.Is(err, business.ErrNotFound):
		unlockAnswer(w, http.StatusNotFound, helpers.Envolope{"msg": "There's no page here."})
	default:
		helpers.LogErrorWithContext(ctx, "controllers/Publish/unlockPageAt: %+v", err)
		unlockAnswer(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "That couldn't be checked just now. Try again in a moment."})
	}
}

// fileName is a stored file's own name: its key's last part, after the id it
// was stored under ("<id>_<name>").
func fileName(objKey string) string {
	name := path.Base(objKey)
	if len(name) > 37 && name[36] == '_' {
		name = name[37:]
	}
	if name == "" || name == "." || name == "/" {
		return "file"
	}
	return name
}

// PublishedFile GET /public/p/{slug}/files/{obj_uuid}: one of a published
// page's own files (D-25, condition 3): a picture as it is, anything else as
// a download. ?k= is a password page's unlock.
func PublishedFile(w http.ResponseWriter, r *http.Request) { serveFile(w, r, bySlug(r)) }

// LinkFile GET /public/d/{token}/files/{obj_uuid}: the same, for a link's page.
func LinkFile(w http.ResponseWriter, r *http.Request) { serveFile(w, r, byToken(r)) }

func serveFile(w http.ResponseWriter, r *http.Request, ref business.Ref) {
	ctx := r.Context()
	if !addressAllowed(ctx, fileSurface, clientIP(r), fileAsksPerAddress).Allowed {
		plain(w, http.StatusTooManyRequests, "Too many requests from here. Wait a minute and try again.")
		return
	}
	f, err := fileFor(ctx, ref, chi.URLParam(r, "obj_uuid"), r.URL.Query().Get("k"))
	switch {
	case err == nil:
	case errors.Is(err, business.ErrNotFound), errors.Is(err, business.ErrLocked):
		plain(w, http.StatusNotFound, "There's no file here.")
		return
	case errors.Is(err, business.ErrGone):
		plain(w, http.StatusGone, "This page has expired.")
		return
	default:
		helpers.LogErrorWithContext(ctx, "controllers/Publish/serveFile: %+v", err)
		plain(w, http.StatusServiceUnavailable, "This file couldn't be fetched just now.")
		return
	}

	h := w.Header()
	common(h, f.Indexable)
	h.Set("Content-Security-Policy", filePolicy)
	// The page that shows a picture has an origin of its own ("null"), so
	// the API's same-site resource policy would refuse it every picture.
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	if f.Private {
		h.Set("Cache-Control", "private, no-store")
	} else {
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", f.ETag)
		if etagMatches(r.Header.Get("If-None-Match"), f.ETag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	stored, err := openStored(ctx, f.ObjKey)
	if errors.Is(err, errNoStoredFile) {
		h.Del("ETag")
		plain(w, http.StatusNotFound, "There's no file here.")
		return
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "controllers/Publish/serveFile storage: %+v", err)
		h.Del("ETag")
		plain(w, http.StatusServiceUnavailable, "This file couldn't be fetched just now.")
		return
	}
	defer stored.Body.Close()
	kind, _, _ := mime.ParseMediaType(stored.ContentType)
	if rasterTypes[strings.ToLower(kind)] {
		h.Set("Content-Type", strings.ToLower(kind))
		h.Set("Content-Disposition", "inline")
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fileName(f.ObjKey)}))
	}
	http.ServeContent(w, r, "", stored.Modified, stored.Body)
}

// visitOf is what counting a visitor reads of the request: the address only
// to hash it, the browser, and the visitor's privacy signals.
func visitOf(r *http.Request) viewBusiness.Visit {
	return viewBusiness.Visit{
		Address:   clientIP(r),
		UserAgent: r.UserAgent(),
		GPC:       strings.TrimSpace(r.Header.Get("Sec-GPC")) == "1",
		DNT:       strings.TrimSpace(r.Header.Get("DNT")) == "1",
	}
}
