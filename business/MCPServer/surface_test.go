package business

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	pendingModels "github.com/akashc777/OneCamp/models/postgres/PendingAction"
)

// declaredSurfaces is the column's whole vocabulary, per migration 101 and the model's
// constants. Anything else in that column is out of contract.
var declaredSurfaces = map[string]bool{
	pendingModels.SurfaceChannel:   true,
	pendingModels.SurfaceDM:        true,
	pendingModels.SurfaceGroup:     true,
	pendingModels.SurfaceAssistant: true,
}

const principal = "11111111-1111-1111-1111-111111111111"

// THE INVARIANT. surface_type has a declared vocabulary, and this package was writing
// resource kinds into it. Every kind — present and future — must map onto one of the four,
// or the column silently stops meaning what its own contract says.
func TestEveryResourceKindMapsToADeclaredSurface(t *testing.T) {
	kinds := []ResourceKind{
		ResourceChannel, ResourceTask, ResourceDoc, ResourceChat, ResourceTable,
		ResourceProject, ResourceTeam, ResourceDataSource, ResourceDirectMessage,
		ResourceSelfOwned, ResourceWorkspace,
	}

	for _, kind := range kinds {
		t.Run(string(kind), func(t *testing.T) {
			surface, _ := SurfaceForResource(ResourceRef{Kind: kind, ID: "some-id"}, principal)
			if !declaredSurfaces[surface] {
				t.Fatalf("%s maps to surface %q, which is not one of channel/dm/group/"+
					"assistant. That column's vocabulary is declared in migration 101 and in "+
					"the frontend's type; a value outside it misroutes anything that switches "+
					"on it and makes the documented union false.", kind, surface)
			}
		})
	}

	// A kind nobody thought about must still land somewhere legal rather than leaking its
	// own name into the column.
	surface, id := SurfaceForResource(ResourceRef{Kind: ResourceKind("something_new"), ID: "x"}, principal)
	if !declaredSurfaces[surface] {
		t.Errorf("an unrecognised kind mapped to %q; the default must be a declared surface", surface)
	}
	if id != "" {
		t.Errorf("an unrecognised kind produced surface id %q; with no thread to point at, it must be empty", id)
	}
}

// A CHAT WRITE MUST LAND IN THE CONVERSATION, which is the point of the mapping beyond
// correctness. The in-thread tray is keyed by grouping id, so this is what makes an approval
// appear where a person would look for it.
func TestChatWritesLandInTheConversationTheyAreAbout(t *testing.T) {
	t.Run("group chat keeps its grouping id", func(t *testing.T) {
		surface, id := SurfaceForResource(ResourceRef{Kind: ResourceChat, ID: "grp-123"}, principal)
		if surface != pendingModels.SurfaceGroup {
			t.Fatalf("group chat mapped to %q, want %q", surface, pendingModels.SurfaceGroup)
		}
		if id != "grp-123" {
			t.Fatalf("grouping id was altered: %q", id)
		}
	})

	t.Run("a DM derives the grouping id from both participants", func(t *testing.T) {
		recipient := "22222222-2222-2222-2222-222222222222"
		surface, id := SurfaceForResource(ResourceRef{Kind: ResourceDirectMessage, ID: recipient}, principal)
		if surface != pendingModels.SurfaceDM {
			t.Fatalf("DM mapped to %q, want %q", surface, pendingModels.SurfaceDM)
		}
		// MUST equal what the frontend computes, or the tray never finds the card. Same
		// rule, same helper.
		want := helpers.GetGroupingId(principal, recipient)
		if id != want {
			t.Fatalf("derived grouping id %q, but the tray looks for %q. The ref names the "+
				"OTHER PERSON, so storing it raw would match no thread.", id, want)
		}
	})

	t.Run("the derived id does not depend on argument order", func(t *testing.T) {
		a, b := "aaaa", "bbbb"
		_, first := SurfaceForResource(ResourceRef{Kind: ResourceDirectMessage, ID: b}, a)
		_, second := SurfaceForResource(ResourceRef{Kind: ResourceDirectMessage, ID: a}, b)
		if first != second {
			t.Fatalf("grouping id depends on who is asking (%q vs %q); the same conversation "+
				"would get two different cards", first, second)
		}
	})

	t.Run("a channel write keeps the channel id", func(t *testing.T) {
		surface, id := SurfaceForResource(ResourceRef{Kind: ResourceChannel, ID: "ch-1"}, principal)
		if surface != pendingModels.SurfaceChannel || id != "ch-1" {
			t.Fatalf("channel mapped to (%q, %q)", surface, id)
		}
	})
}

// Kinds with no thread must say so, rather than pointing at something that renders nowhere.
func TestThreadlessWritesGoToTheAssistantSurface(t *testing.T) {
	for _, kind := range []ResourceKind{
		ResourceTask, ResourceDoc, ResourceTable, ResourceProject,
		ResourceTeam, ResourceDataSource, ResourceSelfOwned, ResourceWorkspace,
	} {
		surface, id := SurfaceForResource(ResourceRef{Kind: kind, ID: "obj-1"}, principal)
		if surface != pendingModels.SurfaceAssistant {
			t.Errorf("%s mapped to %q; it did not arrive in a thread, so there is no thread "+
				"to show its approval in", kind, surface)
		}
		if id != "" {
			t.Errorf("%s produced surface id %q. Pointing the card at an object id makes the "+
				"in-thread tray look for a thread that does not exist, so it renders nowhere",
				kind, id)
		}
	}
}

// Degenerate input must degrade to the assistant surface rather than storing a half-formed
// id that would match no thread.
func TestIncompleteReferencesDegradeToTheAssistantSurface(t *testing.T) {
	cases := []struct {
		name      string
		ref       ResourceRef
		principal string
	}{
		{"channel with no id", ResourceRef{Kind: ResourceChannel}, principal},
		{"chat with no id", ResourceRef{Kind: ResourceChat, ID: "  "}, principal},
		{"DM with no recipient", ResourceRef{Kind: ResourceDirectMessage}, principal},
		{"DM with no principal", ResourceRef{Kind: ResourceDirectMessage, ID: "u2"}, ""},
		{"DM with blank principal", ResourceRef{Kind: ResourceDirectMessage, ID: "u2"}, "   "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			surface, id := SurfaceForResource(tc.ref, tc.principal)
			if surface != pendingModels.SurfaceAssistant || id != "" {
				t.Fatalf("got (%q, %q); an incomplete reference must fall back to the "+
					"assistant surface with no id, not store something that matches no thread",
					surface, id)
			}
		})
	}
}

// THE RATCHET. Neither writer may pass a resource kind into that column again. Both call
// sites are source-checked because the mistake is invisible at runtime: nothing crashes, the
// card just carries a value its own contract forbids.
func TestNoWriterPutsAResourceKindInTheSurfaceColumn(t *testing.T) {
	for _, file := range []string{"claim.go", "pending.go"} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		src := regexp.MustCompile(`//[^\n]*`).ReplaceAllString(string(raw), " ")

		if !strings.Contains(src, "SurfaceForResource(") {
			t.Errorf("%s does not map the resource onto a declared surface. surface_type's "+
				"vocabulary is channel/dm/group/assistant; writing a ResourceKind there puts "+
				"the column out of contract and stops a chat approval reaching its thread.", file)
		}
		if strings.Contains(src, "string(call.Resource.Kind)") {
			t.Errorf("%s still passes string(call.Resource.Kind) as a surface type. That is "+
				"the exact bug: task, table, team, data_source, self_owned and the rest are "+
				"not legal values for that column.", file)
		}
	}
}
