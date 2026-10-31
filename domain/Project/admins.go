package Domain

import (
	"context"
	"strings"

	"github.com/akashc777/OneCamp/helpers/dgraphquery"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// ProjectAdmins is a project's name and the user ids of its admins who are
// people, not bots, active or not.
type ProjectAdmins struct {
	Name   string
	Admins []string
}

// AdminsOf reads, for each of the projects (by id, keyed in lower case),
// its name and its admins who are people: whom a notice about the project
// is for when it is for whoever runs it. One read; an id that isn't a uuid,
// or a project that doesn't exist, is left out.
func AdminsOf(ctx context.Context, projectUUIDs []string) (map[string]ProjectAdmins, error) {
	out := map[string]ProjectAdmins{}
	list := dgraphquery.UUIDList(projectUUIDs)
	if list == "" {
		return out, nil
	}
	type person struct {
		UUID string `json:"user_uuid"`
	}
	rows, err := dgexec.List[struct {
		UUID   string   `json:"project_uuid"`
		Name   string   `json:"project_name"`
		Admins []person `json:"project_admins"`
	}](ctx, `{
		p(func: eq(project_uuid, [`+list+`])) {
			project_uuid
			project_name
			project_admins @filter(NOT eq(is_bot, true)) { user_uuid }
		}
	}`, nil, "p")
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		p := ProjectAdmins{Name: r.Name}
		for _, a := range r.Admins {
			if a.UUID != "" {
				p.Admins = append(p.Admins, a.UUID)
			}
		}
		out[strings.ToLower(r.UUID)] = p
	}
	return out, nil
}
