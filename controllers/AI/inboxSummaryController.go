package controllers

// The inbox's AI summary lives here, not beside the inbox, so the inbox itself
// carries no AI code into the AI-free edition.

import (
	"net/http"
	"strings"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	connectorController "github.com/akashc777/OneCamp/controllers/Connector"
	"github.com/akashc777/OneCamp/helpers"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/go-chi/chi/v5"
)

const inboxSummaryPrompt = `Summarise this email conversation for the person who received it.
In at most five short bullet points: what it is about, what is being asked of them, any dates or amounts, and what they should do next.
If nothing is asked of them, say so. Do not invent anything that is not in the emails.`

// SummarizeInboxThread handles POST /connectors/gmail/threads/{id}/summary.
// Runs on the model routed for summaries, under the workspace's AI rules
// (local-only mode, redaction) like every other summary.
func SummarizeInboxThread(w http.ResponseWriter, r *http.Request) {
	uid, ok := connectorController.InboxUser(w, r)
	if !ok {
		return
	}
	svc := ai.GetService()
	if !svc.IsEnabled() {
		helpers.WriteJSON(w, http.StatusServiceUnavailable, helpers.Envolope{"msg": "AI is not set up on this workspace."})
		return
	}
	d, err := connectorBusiness.GmailThread(r.Context(), uid, chi.URLParam(r, "id"))
	if err != nil {
		connectorController.InboxFail(w, r, err, "summary")
		return
	}
	text := connectorBusiness.ThreadText(d)
	if len(text) > 60000 {
		text = text[:60000]
	}
	summary, err := svc.SummarizeFor(r.Context(), ai.PurposeSummaries, text, inboxSummaryPrompt)
	if err != nil {
		helpers.LogErrorWithContext(r.Context(), "controllers/inbox summary: %v", err)
		helpers.WriteJSON(w, http.StatusBadGateway, helpers.Envolope{"msg": "The summary did not come back. Try again."})
		return
	}
	helpers.WriteJSON(w, http.StatusOK, helpers.Envolope{"data": map[string]string{"summary": strings.TrimSpace(summary)}})
}
