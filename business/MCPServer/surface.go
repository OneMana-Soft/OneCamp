package business

// WHERE an MCP write's approval card belongs.
//
// THE BUG THIS FIXES. ai_pending_actions.surface_type has a declared vocabulary — channel,
// dm, group, assistant — stated in migration 101, in the model's constants, and in the
// frontend's type. This package was passing string(ResourceKind) straight into that column,
// so it wrote task, table, team, data_source, self_owned, direct_message, project, doc,
// chat and workspace. Exactly one of those, channel, is a legal value.
//
// Nothing crashed, because the in-thread tray filters on surface_id rather than
// surface_type, and the home attention list is surface-agnostic. But the column was
// carrying values its own contract forbids: anything that ever switches on surface_type
// would misroute, and the frontend's documented union was simply false about what it could
// receive. A column with a stated vocabulary either holds it or does not have one.
//
// AND THERE IS A REAL GAIN, not only a correctness one. The in-thread approval tray is
// mounted in DM and group-chat views, keyed by the CHAT GROUPING ID. Mapping a chat write
// onto its grouping id means an approval for send_dm or send_group_chat appears in the very
// conversation it is about, which is where a person would look for it. With a resource kind
// in the column and a recipient uuid in surface_id, it appeared only in the home list.
//
// A DM's grouping id is derived from the two participants, sorted and joined — the same
// rule helpers.GetGroupingId applies and the frontend's getGroupingId mirrors character for
// character. Computing it here rather than storing something else is what lets the existing
// tray find the card with no frontend change.
//
// EVERYTHING ELSE IS assistant, which is the honest answer rather than a fallback: a write
// against a task, a table or a document did not arrive in any thread, so there is no thread
// to show it in. Those surface in the home attention list, which draws on every open
// approval regardless of surface.

import (
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
)

// SurfaceForResource maps the object a call touches onto the surface its approval card
// belongs on, and the id that surface is keyed by.
//
// Returns one of the four declared surface types, always — never a resource kind. The
// second value is the surface's own id, which is empty for the assistant surface because it
// is not tied to anything.
//
// principalUserID is needed only for a DM, whose grouping id is derived from both
// participants. Callers pass the accountable person; an unusable one degrades to the
// assistant surface rather than storing a half-formed id that would match no thread.
func SurfaceForResource(ref ResourceRef, principalUserID string) (surfaceType string, surfaceID string) {
	id := strings.TrimSpace(ref.ID)

	switch ref.Kind {
	case ResourceChannel:
		if id == "" {
			break
		}
		return pendingModels.SurfaceChannel, id

	case ResourceChat:
		// A grouping id already, for both a group chat and a DM addressed by grouping.
		if id == "" {
			break
		}
		return pendingModels.SurfaceGroup, id

	case ResourceDirectMessage:
		// The ref names the OTHER PERSON, so the conversation's id has to be derived. Doing
		// that here is what puts the card in the DM thread the tray actually renders.
		principal := strings.TrimSpace(principalUserID)
		if id == "" || principal == "" {
			break
		}
		return pendingModels.SurfaceDM, helpers.GetGroupingId(principal, id)
	}

	// No thread this arrived in: a task, a table, a document, a team, a data source, a
	// workspace-scoped call, or something the caller creates for themselves. The home
	// attention list shows these, since it draws on every open approval for the person
	// regardless of surface.
	return pendingModels.SurfaceAssistant, ""
}
