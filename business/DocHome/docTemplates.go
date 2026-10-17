package business

// Templates on docs that exist (docs programme B5): an empty doc takes a
// template's body (its empty starter offers them), and a doc becomes a
// template the team can start from ("Save as template" in More).
//
// A template's body is written by the server, through the live doc's seed
// (A4): one transaction on the copy everyone has open, refused once the doc
// holds anything, so a second click or a second tab can never add it twice
// (N37). Who may do either is the Access Resolver's to say.

import (
	"context"
	"errors"
	"strings"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	marketplaceBusiness "github.com/akashc777/OneCamp/business/Marketplace"
	model "github.com/akashc777/OneCamp/models/postgres/Marketplace"
	"github.com/akashc777/OneCamp/services/collab"
	"github.com/google/uuid"
)

var (
	// ErrNotFound is a doc that isn't there for the person.
	ErrNotFound = errors.New("there's no such doc")
	// ErrDeleted is a doc in the trash that was there for the person.
	ErrDeleted = errors.New("this doc was deleted")
	// ErrForbidden is a doc the person may open but not change.
	ErrForbidden = errors.New("you can't change this doc")
	// ErrNotEmpty is a doc that holds something already.
	ErrNotEmpty = errors.New("The doc has something in it already, so the template wasn't added.")
)

// Seams for tests: the live doc's seed and read, and a doc's stored copy.
var (
	seedBody   = docBusiness.SeedBodyLive
	readLive   = func(ctx context.Context, docUUID string) (collab.Current, error) { return collab.Read(ctx, docUUID) }
	readStored = docBusiness.GetDocByDocUUID
)

// decide is the person's level on a doc, or the error that says it isn't
// theirs to open: a deleted doc is gone for the people it was there for
// (its owner) and not found for anyone else.
func decide(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
	d, err := accessBusiness.Rules.Resolve(ctx, who, docRes(docUUID))
	switch {
	case errors.Is(err, docBusiness.ErrNotFound):
		return d, ErrNotFound
	case errors.Is(err, docBusiness.ErrDocDeleted):
		return d, ErrDeleted
	case err != nil:
		return d, err
	case d.Deleted && d.Owner:
		return d, ErrDeleted
	case d.Deleted, d.Level == accessBusiness.LevelNone:
		return d, ErrNotFound
	}
	return d, nil
}

// ApplyTemplate gives an empty doc the person may change a template's body.
// ErrNotEmpty when it holds something already; docBusiness.ErrNoTemplate for
// a template that isn't there.
func ApplyTemplate(ctx context.Context, who accessBusiness.Subject, docUUID, templateID string) (collab.Result, error) {
	d, err := decide(ctx, who, docUUID)
	if err != nil {
		return collab.Result{}, err
	}
	if d.Level < accessBusiness.LevelEdit {
		return collab.Result{}, ErrForbidden
	}
	body, err := docBusiness.Templates.Body(ctx, templateID)
	if err != nil {
		return collab.Result{}, err
	}
	r, err := seedBody(ctx, docUUID, body, collab.By{UserUUID: who.UserUUID})
	if errors.Is(err, collab.ErrHasContent) {
		return r, ErrNotEmpty
	}
	return r, err
}

// SaveAsTemplate makes a doc the person may change into a template for the
// team: its body as the live copy holds it now (typing of a moment ago
// included), else as last stored; its title, and its icon. createdBy is the
// person's account id, whose the template is. Every member sees a template's
// preview, so view or comment access isn't enough: that would let anyone a
// private doc is shared with read-only publish its body (ErrForbidden).
func SaveAsTemplate(ctx context.Context, who accessBusiness.Subject, createdBy uuid.UUID, docUUID, name, description string) (*model.Template, error) {
	d, err := decide(ctx, who, docUUID)
	if err != nil {
		return nil, err
	}
	if d.Level < accessBusiness.LevelEdit {
		return nil, ErrForbidden
	}
	doc, err := readStored(ctx, docUUID, who.UserUID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, ErrNotFound
	}
	body := doc.Body
	if cur, err := readLive(ctx, docUUID); err == nil {
		body = cur.HTML
	}
	if strings.TrimSpace(name) == "" {
		name = doc.Title
	}
	return marketplaceBusiness.PublishDocTemplate(ctx, createdBy, name, description, doc.Icon, marketplaceBusiness.DocPayload{
		Title: doc.Title, Body: body, Hue: marketplaceBusiness.HueFor(docUUID), Glyph: "file-text",
	})
}
