package business

// A doc's entry in the AI index follows its saved body (docs programme A4; the
// AI edition's part of a save). It is a save hook (business/Doc SaveHooks), so
// it runs only after a save that CHANGED the body, in the background, and the
// shared save path (business/Doc docCollabSave.go) holds no AI code: the
// edition without AI has no hook and nothing to strip.
//
// THE TITLE AND WHO MAY SEE IT come from the doc as stored (one read, its title,
// privacy and sharing), never from the save, which carries the body alone:
// embedding only what a save carried dropped the title from the doc's vector,
// and indexing it as public put a private doc in everyone's AI search.

import (
	"context"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	docDomain "github.com/akashc777/OneCamp/domain/Doc"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
)

func init() { docBusiness.SaveHooks.Register("ai.embed", embedSavedDoc) }

// Seams, so the hook is tested without the graph or the index.
var (
	readDocForEmbed = docDomain.GetDocPermissions
	embedDoc        = ai.EmbedDocContent
)

// embedSavedDoc re-embeds a doc whose body a save changed: its title and the
// saved body as plain text (markup would spend the model's input on tags, and
// on base64 when an image was inlined), with its privacy and sharing.
func embedSavedDoc(ctx context.Context, c docBusiness.Change) {
	doc, err := readDocForEmbed(ctx, c.DocUUID)
	if err != nil || doc == nil {
		helpers.LogErrorWithContext(ctx, "business/AI embedSavedDoc doc %s: could not read it to re-embed: %v", c.DocUUID, err)
		return
	}
	if helpers.IsSoftDeleted(doc.DeletedAt) {
		// Deleted since the save: its entry goes with it, not back in.
		return
	}
	a := ai.DocAccessOf(c.DocUUID, doc)
	embedDoc(doc.Title, helpers.HTMLToPlainText(c.Body), c.DocUUID, "", "", a.Private, a.CreatedBy, a.Reading, a.Editing, a.Commenting)
}
