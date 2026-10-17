package business

import (
	"context"
	"errors"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	docExportModels "github.com/akashc777/OneCamp/models/postgres/DocExport"
	"github.com/google/uuid"
)

// A private doc's owner lets it into the admins' workspace exports, or takes
// it back out (workspaceExport.go). Only the owner decides: the consent is
// theirs, and is no longer counted once the doc has another owner.

// Seams for tests: where consents are kept.
var (
	allowExport    = docExportModels.Allow
	withdrawExport = docExportModels.Withdraw
)

// ExportConsentState is where a doc stands with workspace exports, for
// someone who can open it.
type ExportConsentState struct {
	// Allowed is whether its owner has let it into workspace exports.
	Allowed bool `json:"allowed"`
	// Owner is whether the person asking owns it, and so may change that.
	Owner bool `json:"owner"`
	// Private is whether it isn't open to everyone in the workspace, so an
	// admin's export takes it only with its owner's yes.
	Private bool `json:"private"`
}

// ExportConsent says whether docUUID's owner has let it into workspace
// exports. Anyone who can open the doc may ask.
func ExportConsent(ctx context.Context, who accessBusiness.Subject, docUUID string) (ExportConsentState, error) {
	d, err := consentDecision(ctx, who, docUUID)
	if err != nil {
		return ExportConsentState{}, err
	}
	given, err := consentsFor(ctx, []string{docUUID})
	if err != nil {
		return ExportConsentState{}, err
	}
	rows, err := docRowsOf(ctx, []string{docUUID}, who.UserUID)
	if err != nil {
		return ExportConsentState{}, err
	}
	if len(rows) == 0 {
		return ExportConsentState{}, ErrNotFound
	}
	d0 := rows[0]
	return ExportConsentState{
		Allowed: given[docUUID] != "" && given[docUUID] == ownerUUID(d0),
		Owner:   d.Owner,
		Private: docDomain.IsPrivate(d0),
	}, nil
}

// SetExportConsent lets the doc into workspace exports, or takes it back out:
// for its owner only (ErrForbidden for anyone else).
func SetExportConsent(ctx context.Context, who accessBusiness.Subject, docUUID string, allow bool) error {
	d, err := consentDecision(ctx, who, docUUID)
	if err != nil {
		return err
	}
	owner, err := uuid.Parse(who.UserUUID)
	if err != nil || !d.Owner {
		return ErrForbidden
	}
	doc := uuid.MustParse(docUUID)
	if allow {
		return allowExport(ctx, doc, owner)
	}
	return withdrawExport(ctx, doc)
}

// consentDecision is who's decision on a live doc they can open; ErrNotFound
// for any other doc.
func consentDecision(ctx context.Context, who accessBusiness.Subject, docUUID string) (accessBusiness.Decision, error) {
	if _, err := uuid.Parse(docUUID); err != nil {
		return accessBusiness.Decision{}, ErrNotFound
	}
	d, err := accessBusiness.Rules.Resolve(ctx, who, accessBusiness.Resource{Kind: accessBusiness.KindDoc, UUID: docUUID})
	switch {
	case errors.Is(err, docBusiness.ErrNotFound):
		return d, ErrNotFound
	case err != nil:
		return d, err
	case d.Deleted || d.Level < accessBusiness.LevelView:
		return d, ErrNotFound
	}
	return d, nil
}
