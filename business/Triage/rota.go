package business

// The rota (linear-21 H2): who is on a project's triage duty, in turns of a
// number of weeks from a start date. A project's own setting, kept as its
// projects.triage.rota policy value beside projects.triage (no table): read
// per project only, from the scope's cached values the triage switch fills.
// Whoever the queue admits reads it; the project's admins set it, in the app.
// Rules give arriving work to whoever is on duty (business/Workflow on_duty).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	projectBusiness "github.com/akashc777/OneCamp/business/Project"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Rota is who is on a project's triage duty: People in turns of Weeks weeks,
// the first turn starting on Start (a date, YYYY-MM-DD; turns change at
// 00:00 UTC). No one on it is no one on duty.
type Rota struct {
	People []string `json:"people"`
	Weeks  int      `json:"weeks"`
	Start  string   `json:"start"`
}

// RotaView is a rota, who is on duty now ("" for nobody) and when that next
// changes (RFC 3339, UTC; "" for never), for the panel to show in the
// reader's own time. CanManage is whether the reader may set it (written
// beside the rota, not in it).
type RotaView struct {
	Rota
	OnDuty     string `json:"on_duty"`
	NextChange string `json:"next_change"`
	CanManage  bool   `json:"-"`
}

const (
	// MaxRota bounds the people on a rota.
	MaxRota = 50
	// MaxRotaWeeks bounds a turn.
	MaxRotaWeeks = 12
	// ArrivedEvent is what work that comes to wait in triage tells the
	// rules engine (business/Workflow task_arrived): task_id, project_id,
	// source (the door) and updated_by_uuid (who sent it, when known).
	ArrivedEvent = "task.arrived"
)

// ErrBadRota is a rota that can't be kept: someone not in the project, a
// turn outside 1 to MaxRotaWeeks weeks, a start that isn't a date.
var ErrBadRota = errors.New("triage: that rota can't be kept")

// ErrRotaThroughToken is the rota set through an API or MCP token: tokens act
// as members, and the rota is the project admins' to set, in the app.
var ErrRotaThroughToken = errors.New("triage: the rota is set in the app, not through a token")

// Seams for tests.
var (
	readRota = func(ctx context.Context, projectID string) (Rota, error) {
		var r Rota
		_, err := policyBusiness.Policies.ObjectIn(ctx, policyBusiness.ProjectsTriageRota, projectScope(projectID), &r)
		return r, err
	}
	writeRota = func(ctx context.Context, projectID string, r Rota, by policyBusiness.Changer) error {
		raw, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return policyBusiness.Admin.SetObjectIn(ctx, policyBusiness.OwnerTriage, policyBusiness.ProjectsTriageRota, projectScope(projectID), raw, by)
	}
	membersIn    = projectBusiness.MembersAmong
	arrivedEvent = func(ctx context.Context, eventType string, data map[string]interface{}) {
		webhookBusiness.DispatchEvent(ctx, eventType, data)
	}
)

func projectScope(id string) policyBusiness.Scope {
	return policyBusiness.Scope{Kind: policyBusiness.ScopeProject, ID: id}
}

// onDuty is who is on the rota's duty at now: the turn now falls in, counted
// in whole UTC days from the start, round and round. Nobody before the
// start, or on a rota with no one, no length or no start. Pure.
func onDuty(r Rota, now time.Time) (string, time.Time) {
	start, err := time.Parse(time.DateOnly, r.Start)
	if err != nil || len(r.People) == 0 || r.Weeks < 1 {
		return "", time.Time{}
	}
	now = now.UTC()
	if now.Before(start) {
		return "", start
	}
	length := time.Duration(7*r.Weeks) * 24 * time.Hour
	turn := int(now.Sub(start) / length)
	if len(r.People) == 1 {
		return r.People[0], time.Time{}
	}
	return r.People[turn%len(r.People)], start.Add(time.Duration(turn+1) * length)
}

// view is r with who is on duty now and when that changes, people never
// null, for a reader with standing s. Pure.
func view(ctx context.Context, r Rota, s workAccess.Standing) RotaView {
	if r.People == nil {
		r.People = []string{}
	}
	who, next := onDuty(r, now())
	v := RotaView{Rota: r, OnDuty: who, CanManage: s.Can(workAccess.NeedManage) && !helpers.ActsThroughToken(ctx)}
	if !next.IsZero() {
		v.NextChange = next.Format(time.RFC3339)
	}
	return v
}

// ReadRota is a project's rota and who is on duty now, for whoever the queue
// admits; a project that keeps none reads as an empty rota. s is the
// reader's standing in the project as the route checked it: neither the
// project nor the standing is read again.
func ReadRota(ctx context.Context, s workAccess.Standing, projectID uuid.UUID) (RotaView, error) {
	if err := allowed(s, projectID, workAccess.NeedView); err != nil {
		return RotaView{}, err
	}
	r, err := readRota(ctx, projectID.String())
	if err != nil {
		return RotaView{}, err
	}
	return view(ctx, r, s), nil
}

// SetRota keeps a project's rota, for its admins (NeedManage), in the app:
// everyone on it in the project, each once, in their order. Recorded in the
// audit log (business/Policy). An empty rota is a rota: nobody on duty. s
// is who's standing as the route checked it, as for ReadRota.
func SetRota(ctx context.Context, who *userModels.UserInfo, s workAccess.Standing, projectID uuid.UUID, r Rota) (RotaView, error) {
	if helpers.ActsThroughToken(ctx) {
		return RotaView{}, ErrRotaThroughToken
	}
	if err := allowed(s, projectID, workAccess.NeedManage); err != nil {
		return RotaView{}, err
	}
	r, err := shaped(r)
	if err != nil {
		return RotaView{}, err
	}
	if len(r.People) > 0 {
		in, err := membersIn(ctx, projectID.String(), r.People)
		if err != nil {
			return RotaView{}, err
		}
		for _, p := range r.People {
			if !in[p] {
				return RotaView{}, fmt.Errorf("%w: everyone on it is in the project", ErrBadRota)
			}
		}
	}
	by := policyBusiness.Changer{UserID: who.UserPostgresInfo.Id, Email: who.UserPostgresInfo.EmailID}
	if err := writeRota(ctx, projectID.String(), r, by); err != nil {
		return RotaView{}, err
	}
	return view(ctx, r, s), nil
}

// shaped is r as it is kept: each person's id once, in order; a turn of 1 to
// MaxRotaWeeks weeks; a start that is a date. Pure.
func shaped(r Rota) (Rota, error) {
	if len(r.People) > MaxRota {
		return r, fmt.Errorf("%w: at most %d people", ErrBadRota, MaxRota)
	}
	if r.Weeks < 1 || r.Weeks > MaxRotaWeeks {
		return r, fmt.Errorf("%w: a turn is 1 to %d weeks", ErrBadRota, MaxRotaWeeks)
	}
	start, err := time.Parse(time.DateOnly, strings.TrimSpace(r.Start))
	if err != nil {
		return r, fmt.Errorf("%w: the first turn starts on a date, like 2026-11-02", ErrBadRota)
	}
	out := Rota{People: make([]string, 0, len(r.People)), Weeks: r.Weeks, Start: start.Format(time.DateOnly)}
	seen := map[string]bool{}
	for _, p := range r.People {
		id, err := uuid.Parse(strings.TrimSpace(p))
		if err != nil {
			return r, fmt.Errorf("%w: pick people from the project", ErrBadRota)
		}
		if !seen[id.String()] {
			seen[id.String()] = true
			out.People = append(out.People, id.String())
		}
	}
	return out, nil
}

// OnDuty is who is on the project's triage duty now, "" for nobody: for the
// rules engine, which checks the rule's owner itself. One read of the
// project's cached policy values.
func OnDuty(ctx context.Context, projectID string) (string, error) {
	r, err := readRota(ctx, projectID)
	if err != nil {
		return "", err
	}
	who, _ := onDuty(r, now())
	return who, nil
}

// arrived tells the rules engine work came to wait in a project's triage.
func arrived(ctx context.Context, projectID, taskID uuid.UUID, src Source, from Requester) {
	data := map[string]interface{}{"task_id": taskID.String(), "project_id": projectID.String(), "source": string(src)}
	if from.UserUUID != nil {
		data["updated_by_uuid"] = from.UserUUID.String()
	}
	arrivedEvent(ctx, ArrivedEvent, data)
}
