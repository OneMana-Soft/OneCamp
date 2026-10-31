package router

import (
	"github.com/go-chi/chi/v5"

	customMiddleware "github.com/akashc777/OneCamp/middleware"
)

// The docs programme's routes (PROGRAM.md §3.6 and §9 in the programme's design
// folder).
//
// WHY A FILE OF ITS OWN. The programme lands its packages in waves, most with
// routes, and router.go is the file each would otherwise edit at the same time.
// router.go is frozen instead: it calls mountDocsProgramme once, after every other
// route, and each package adds ONE line to mountDocsProgramme, written by the
// integrator when the package merges. A package's own routes go in a file of their
// own in this package, router/docs<Area>.go, as func mount<Area>(rt docsRouters),
// so every route the server answers is still declared under router/, where
// publicSurface_test.go reads them.
//
// THE ROUTERS CARRY THEIR PROTECTION. Authorisation in this router is positional
// (see publicSurface_test.go): a route is exactly as protected as the router it is
// registered on. docsRouters holds only protected routers, and deliberately no
// root router, so nothing mounted through this file can be public.
// TestDocsProgrammeRoutersAreProtected holds that line.
//
// A ROUTE SWAP, where a package replaces a handler in controllers/Doc, deletes the
// old line from router.go and registers the new handler in the package's mount
// function. Never register a method and pattern twice: chi keeps the last handler
// registered without a word, so the old line would stay in the source while
// answering nothing.
type docsRouters struct {
	// Doc is /doc: signed-in members, CSRF-checked, bodies capped at 8 MiB.
	Doc chi.Router
	// Member is the root behind the same middleware as /doc, for an area with a
	// path of its own (/places, /threads, /proposals, /content-io).
	Member chi.Router
	// Admin is /admin: workspace admins only.
	Admin chi.Router
	// Internal is /docColab for routes only the collaboration service calls, behind
	// the shared internal secret.
	Internal chi.Router
	// V1 is the public API's /v1: an API token's owner, each route asserting the
	// scope it needs (customMiddleware.RequireScope).
	V1 chi.Router
	// Files is the root behind /doc's middleware but its 8 MiB body cap, for
	// the routes that take a file upload and nothing else (docs F1: a file in
	// the doc tree, and its new versions): a file under the workspace's upload
	// limit (10 MB by default, up to 5 GB) may be larger than 8 MiB. Each of
	// its handlers caps the body at that limit before reading it
	// (httpx.ReadUpload), as /uploadFile's does.
	Files chi.Router
}

// newDocsRouters builds the routers a package may mount on. doc, admin,
// docColab and v1 are router.go's /doc, /admin, /docColab and /v1 sub-routers;
// root is the top-level router, used here only to open the authenticated
// member group.
func newDocsRouters(root, doc, admin, docColab, v1 chi.Router) docsRouters {
	return docsRouters{
		Doc:      doc,
		Member:   membersOnly(root),
		Admin:    admin,
		Internal: docColab.With(customMiddleware.VerifyInternalServiceRequest),
		V1:       v1,
		Files:    uploadsOnly(root),
	}
}

// uploadsOnly is root behind the member group's middleware but its body cap
// (CSRF, a signed-in member): the Files router, whose handlers cap their
// uploads at the workspace's limit themselves.
func uploadsOnly(root chi.Router) chi.Router {
	return root.With(
		customMiddleware.CSRFMiddleware,
		customMiddleware.VerifyAuth,
	)
}

// membersOnly is root behind the middleware router.go's member group carries
// (CSRF, a signed-in member, bodies capped at 8 MiB): the Member router every
// programme (docs, chat, calendar) hands its packages.
func membersOnly(root chi.Router) chi.Router {
	return root.With(
		customMiddleware.CSRFMiddleware,
		customMiddleware.VerifyAuth,
		customMiddleware.BodyLimit(8<<20),
	)
}

// mountDocsProgramme mounts every merged package of the programme: one line per
// package, in merge order (A0 → A9, then B1 → B9, then C1 → C12), and nothing
// else.
func mountDocsProgramme(rt docsRouters) {
	mountProposals(rt)   // A9: edit proposals
	mountDocTree(rt)     // A3: the doc tree
	mountDocLists(rt)    // A3: doc lists
	mountDocTrash(rt)    // A3: trash
	mountPlaces(rt)      // A3: stars and pins
	mountContentIO(rt)   // A7: import and export
	mountThreads(rt)     // A8: anchored threads
	mountDocsSession(rt) // A2: the doc session's authorisation
	mountDocVersions(rt) // A4: versions, and saves through the live doc
	mountFollows(rt)     // A6: follows
	mountPolicies(rt)    // A6: the policy registry, for admins
	mountDocAccess(rt)   // A5: access, sharing, the lock and requests
	mountUnfurl(rt)      // B4: link previews, embed sites, pictures kept from a link
	mountDocBlocks(rt)   // B1: blocks moved to another doc
	mountDocsHome(rt)    // B5: search in docs' bodies, and doc templates
	mountDocAI(rt)       // B9: doc AI (AI edition only)
	mountDocChrome(rt)   // B6: page chrome and sharing
	// C1: live chips, who an "@" reaches, to-dos that are tasks
	mountConnectedWork(rt)

	mountDocsMigration(rt) // C4: the workspace export and its consents; reading other tools' exports
	// C4's docs in the public API register in controllers/V1/routes.go.
	mountQuery(rt)       // C2: the one query grammar, read for the signed-in person
	mountPublish(rt)     // C6: docs published to the web
	mountGuestThread(rt) // C5-2: a doc's conversation with its guests, and its client verdict
	mountDocFiles(rt)    // F1: Docs & Files' files and their versions
	mountDocsAdmin(rt)   // C6-2: the work of people who left, and the link lifetime's count
}
