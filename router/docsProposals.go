package router

import (
	proposalController "github.com/akashc777/OneCamp/controllers/Proposal"
)

// mountProposals is A9's routes: edit proposals (business/Proposal).
//
// /proposals is for signed-in members (each call decides access through the
// Access Resolver); /docColab/proposals is for the collaboration service's
// applyProposal op only, behind the internal secret.
func mountProposals(rt docsRouters) {
	rt.Member.Post("/proposals", proposalController.CreateProposal)
	rt.Member.Get("/proposals", proposalController.ListProposals)
	rt.Member.Get("/proposals/{proposal_id}", proposalController.GetProposal)
	rt.Member.Post("/proposals/{proposal_id}/decide", proposalController.DecideProposal)
	rt.Member.Post("/proposals/{proposal_id}/withdraw", proposalController.WithdrawProposal)
	rt.Member.Post("/proposals/{proposal_id}/reopen", proposalController.ReopenProposal)

	rt.Internal.Get("/proposals/{proposal_id}", proposalController.ProposalStep)
	rt.Internal.Post("/proposals/{proposal_id}/prepared", proposalController.ProposalPrepared)
}
