package basecamp

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
)

// RecheckPeople reads, with owner's stored sign-in, which of projects (source
// ids) answer in the job's account (one read each; a 404 is a project
// deleted, or hidden from owner), and the account's people, each kept out of
// projects or not by the import's own rule (joinsProjects). A sign-in that
// doesn't see the job's account answers for no project.
func (p *Provider) RecheckPeople(ctx context.Context, owner uuid.UUID, j *importModels.Job, projects []string) (importProvider.RecheckedPeople, error) {
	tok, err := tokenFor(ctx, owner)
	if err != nil {
		return importProvider.RecheckedPeople{}, err
	}
	return p.recheckPeople(ctx, tok, j, projects)
}

// recheckPeople is RecheckPeople with the sign-in's token read.
func (p *Provider) recheckPeople(ctx context.Context, tok string, j *importModels.Job, projects []string) (importProvider.RecheckedPeople, error) {
	now := importProvider.RecheckedPeople{Answered: map[string]bool{}, KeptOut: map[string]bool{}}
	base, err := p.pickAccount(ctx, tok, j)
	if errors.As(err, new(errUnpicked)) {
		return now, nil
	}
	if err != nil {
		return now, err
	}
	for _, project := range projects {
		// A source id the import recorded is Basecamp's number; anything else
		// names no project of the account.
		if _, err := strconv.ParseInt(project, 10, 64); err != nil {
			continue
		}
		var pr bcProject
		_, err := p.get(ctx, tok, base+"/projects/"+project+".json", &pr)
		var gone *importProvider.StatusError
		if errors.As(err, &gone) && gone.Code == http.StatusNotFound {
			continue
		}
		if err != nil {
			return now, err
		}
		now.Answered[project] = true
	}
	people, err := p.people(ctx, tok, base+"/people.json")
	if err != nil {
		return now, err
	}
	for _, pr := range people {
		now.KeptOut[id(pr.ID)] = !joinsProjects(pr)
	}
	return now, nil
}
