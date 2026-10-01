package business

// The recap as a document, not only as a message.
//
// WHY BOTH. A message is a notification: it tells the channel a call happened
// and what came out of it, and then it scrolls away. Notes are a different
// thing. People edit them, correct the name the model misheard, add the decision
// that was made after the call ended, and come back to them a month later. A
// message cannot be edited by the people who were in the room, and it cannot
// carry a full transcript without burying the channel it was posted to.
//
// So the message stays exactly as it was and the document is added beside it.
// Nothing that worked before works differently.
//
// WHO CAN SEE IT. Private, with the speakers granted edit access. That is
// deliberately NARROWER than the channel the recap is posted to: it cannot leak
// a private call to a workspace, which the alternative (doc_private = false,
// the only other primitive here) would do by listing it to everyone. The cost
// is that somebody who attended and never spoke does not get it, and the person
// who did can share it. Being too narrow is recoverable by a human; being too
// wide is not.

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	docAdapter "github.com/akashc777/OneCamp/adapter/Doc"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	channelDomain "github.com/akashc777/OneCamp/domain/Channel"
	transcriptDomain "github.com/akashc777/OneCamp/domain/LiveKit"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/akashc777/OneCamp/models/redis/registry"
	redisStore "github.com/akashc777/OneCamp/models/redis/store"
)

// maxDocTranscriptLines bounds what goes into the document body.
//
// The document holds the WHOLE transcript, which is the point of it existing,
// but a body is stored and re-embedded on every edit, so it cannot be unbounded.
// A four-hour call at conversational pace lands well inside this.
const maxDocTranscriptLines = 3000

// buildMeetingNotesHTML renders the notes body: the recap first, because that is
// what anyone opening it wants, then the transcript underneath.
//
// Everything is escaped. The transcript is speech transcribed by a model from
// audio supplied by whoever was on the call, so it is untrusted input on its way
// into a document other people will open.
func buildMeetingNotesHTML(recap, gapNote string, lines []transcriptDomain.TranscriptLine, nameByUID map[string]string) string {
	var sb strings.Builder

	sb.WriteString(modelTextToHTML(recap))

	sb.WriteString("<h2>Transcript</h2>")
	// Said here as well as on the message, because the document is the thing
	// people come back to and the message is the thing they scroll past.
	if note := strings.TrimSpace(gapNote); note != "" {
		sb.WriteString("<p><em>" + html.EscapeString(note) + "</em></p>")
	}
	if len(lines) > maxDocTranscriptLines {
		sb.WriteString(fmt.Sprintf(
			"<p><em>Showing the first %d lines of %d.</em></p>", maxDocTranscriptLines, len(lines)))
		lines = lines[:maxDocTranscriptLines]
	}
	for _, l := range lines {
		text := strings.TrimSpace(l.Text)
		if text == "" {
			continue
		}
		speaker := nameByUID[l.ParticipantIdentity]
		if strings.TrimSpace(speaker) == "" {
			// A speaker who has left the workspace still said something. Naming
			// them "Unknown" is better than attributing their words to nobody,
			// and far better than leaving a raw uid in a document.
			speaker = "Unknown speaker"
		}
		sb.WriteString("<p><strong>" + html.EscapeString(speaker) + ":</strong> " + html.EscapeString(text) + "</p>")
	}
	// The document body is machine-written too, and a document is the thing
	// most likely to be exported or shared outside the workspace.
	return MarkAIGenerated(sb.String())
}

// meetingNotesTitle names the document so a list of them is readable.
func meetingNotesTitle(surfaceName string, at time.Time) string {
	when := at.Format("2 Jan 2006, 15:04")
	if s := strings.TrimSpace(surfaceName); s != "" {
		return fmt.Sprintf("Meeting notes: %s, %s", s, when)
	}
	return "Meeting notes: " + when
}

// notesAudience is everyone the document should reach: the people who were in
// the call, falling back to the people who spoke.
//
// Speakers alone was the safe answer while nothing recorded attendance, and it
// was wrong in a way people notice: sit through an hour, say nothing, and the
// notes are not yours. participant_left now records everyone, so the ordinary
// case is the right set.
//
// Speakers are ALWAYS included even when attendance is known, because the two
// are recorded by different mechanisms and only one of them is proof. A speaker
// is in the transcript because they talked; if Redis dropped the set, or the
// room was renamed, or the webhook never arrived, the transcript is still
// evidence they were there.
func notesAudience(ctx context.Context, roomName string, speakerUIDs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(speakerUIDs)+4)
	add := func(uid string) {
		uid = strings.TrimSpace(uid)
		if uid == "" || seen[uid] {
			return
		}
		seen[uid] = true
		out = append(out, uid)
	}

	// Speakers first: the owner is picked from the head of this list, and an
	// owner who spoke is the likeliest to still be a live user.
	for _, uid := range speakerUIDs {
		add(uid)
	}

	attendees, err := redisStore.GetSetMembers(ctx, registry.CallParticipants, []string{roomName})
	if err != nil {
		// Degrade to speakers rather than failing: fewer people on the document
		// is recoverable, no document is not.
		helpers.LogErrorWithContext(ctx, "AI meeting notes: could not read call participants for %q: %+v", roomName, err)
		return out
	}
	for _, uid := range attendees {
		add(uid)
	}
	return out
}

// resolveNotesOwner picks the document's owner: the first speaker who still
// resolves to a live user.
//
// Tries in order for the same reason the recap's delivery does. A speaker may
// have left the workspace between the call and the recap, and the owner is the
// one uid the document cannot be created without, so failing on the first
// candidate would lose the document for a reason that has nothing to do with it.
//
// Bounded, so a pathological speaker list cannot fan out into many lookups.
func resolveNotesOwner(ctx context.Context, speakerUIDs []string) *dgraphStruct.DgraphUser {
	tried := 0
	for _, uid := range speakerUIDs {
		if tried >= 5 {
			break
		}
		if strings.TrimSpace(uid) == "" {
			continue
		}
		tried++
		if u, err := userDomain.GetDgraphUserInfoByDgraphUID(ctx, uid); err == nil && u != nil && u.Uid != "" {
			return u
		}
	}
	return nil
}

// meetingSurfaceName is the readable label for the call's surface, used in the
// document title so a list of meeting notes is scannable.
//
// Returns "" for anything that is not a channel, which the title falls back
// from cleanly. A DM's participants are exactly the people the document is
// shared with, so naming them in the title would tell them only what they
// already know, and would put someone's name in a title on a page they may
// later share.
func meetingSurfaceName(ctx context.Context, roomName, viewerUID string) string {
	channelUUID, _ := helpers.ClassifyRoom(roomName)
	if channelUUID == "" {
		return ""
	}
	ch, err := channelDomain.GetBasicDgraphChannelInfoByUUID(ctx, channelUUID, viewerUID)
	if err != nil || ch == nil || strings.TrimSpace(ch.Name) == "" {
		// A title without the channel is still a usable title.
		return ""
	}
	return "#" + ch.Name
}

// createMeetingNotesDoc writes the notes document and returns its uuid.
//
// Best-effort by contract: the recap message is the thing that must not fail, so
// every caller treats an error here as "no document this time" and carries on.
// Returns "" with no error when there is nobody to own it, because a document
// whose editing set is empty is one nobody can ever open.
func createMeetingNotesDoc(
	ctx context.Context,
	surfaceName, roomName string,
	speakerUIDs []string,
	recap, gapNote string,
	lines []transcriptDomain.TranscriptLine,
	nameByUID map[string]string,
) (string, error) {
	audience := notesAudience(ctx, roomName, speakerUIDs)
	author := resolveNotesOwner(ctx, audience)
	if author == nil {
		// Nobody on the call resolves to a live user, so there is no one to own
		// the document and nobody who could open it. Not an error: the recap
		// still posts, which is the part that matters.
		return "", nil
	}

	doc, err := docBusiness.CreateDoc(ctx, author, &docAdapter.InputCreateDoc{
		DocTitle:   meetingNotesTitle(surfaceName, time.Now()),
		DocPrivate: true,
	}, audience...)
	if err != nil {
		return "", err
	}
	if doc == nil {
		return "", fmt.Errorf("meeting notes document was not created")
	}

	body := buildMeetingNotesHTML(recap, gapNote, lines, nameByUID)
	if err := docBusiness.UpdateDoc(ctx, &docAdapter.InputUpdateDoc{DocId: doc.Uuid, Body: &body}); err != nil {
		// The document exists but is empty. Reported rather than swallowed,
		// because an empty "Meeting notes" document is a worse outcome than
		// none and somebody should see it in the log.
		return doc.Uuid, fmt.Errorf("write meeting notes body: %w", err)
	}
	return doc.Uuid, nil
}
