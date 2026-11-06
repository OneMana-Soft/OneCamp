package businness

// Docs carry sensitivity labels (miro-44 head L, business/Label): a doc's
// standing for a person is its access rule (access.go: read, edit, owner),
// and its rows go under "docs", never naming a private page, as the doc's
// other rows don't (business/Follow/audit.go).

import (
	"context"
	"errors"
	"strings"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	labelBusiness "github.com/akashc777/OneCamp/business/Label"
)

// LabelKind is a doc, as labels and guest links name it.
const LabelKind = "doc"

func init() {
	labelBusiness.RegisterKind(labelBusiness.Kind{Name: LabelKind, Noun: "doc", Category: auditBusiness.CategoryDocs, Stand: docStanding})
}

// labelDocRead is the doc as the person reads it, with its access facts; a
// seam for tests.
var labelDocRead = GetDgraphDocByUUIDOnlyEditingInfo

// docStanding is who's standing on a doc by its access rule.
func docStanding(ctx context.Context, who labelBusiness.Who, id string) (labelBusiness.Standing, error) {
	d, err := labelDocRead(ctx, id, who.UserUID)
	if errors.Is(err, ErrNotFound) || (err == nil && d == nil) {
		return labelBusiness.Standing{}, labelBusiness.ErrItemNotFound
	}
	if err != nil {
		return labelBusiness.Standing{}, err
	}
	title := strings.TrimSpace(d.Title)
	if title == "" {
		title = "Untitled"
	}
	name := "‘" + title + "’"
	if d.IsPrivate != nil && *d.IsPrivate {
		name = "a private page"
	}
	return labelBusiness.Standing{
		Read: CanRead(d, who.UserUUID),
		Edit: CanEdit(d, who.UserUID),
		Own:  IsOwner(d, who.UserUID),
		Name: name,
	}, nil
}
