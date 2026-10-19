package business

// The hosts of hosted docs (docs D-18, amended): a doc that belongs to
// something else, a table row's page, is decided by its host's rule, the one
// the host's kind registered here (kinds.go), on a path whose handler opens
// hosted docs (business/Doc docHost.go). business/Doc can't ask this package
// (it is imported by it), so it asks through the rule set here.
//
// The host is decided for the person the doc's decision is for, as the
// resolver was asked: ResolveAll puts its subject on the context it decides
// docs with, so a token's cleared admin flag holds for the host too. An agent
// never opens a hosted doc (yet): its decision is none.

import (
	"context"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

func init() { docBusiness.SetHostRule(decideHosts) }

// subjectKey carries the subject ResolveAll was asked for.
type subjectKey struct{}

func withSubject(ctx context.Context, who Subject) context.Context {
	return context.WithValue(ctx, subjectKey{}, who)
}

// subjectFor is who a decision for v is: the resolver's subject when it is
// the one asking for v, else the person v names with no admin power (a
// caller of business/Doc that opened hosted docs itself, by uuid alone).
func subjectFor(ctx context.Context, v docBusiness.Viewer) Subject {
	if who, ok := ctx.Value(subjectKey{}).(Subject); ok && (who.UserUUID == v.UUID || (who.UserUID != "" && who.UserUID == v.UID)) {
		return who
	}
	return Subject{UserUID: v.UID, UserUUID: v.UUID}
}

// decideHosts is business/Doc's HostRule: each host by its kind's rule.
func decideHosts(ctx context.Context, v docBusiness.Viewer, hosts []docBusiness.Host) (map[docBusiness.Host]docBusiness.HostDecision, error) {
	who := subjectFor(ctx, v)
	out := make(map[docBusiness.Host]docBusiness.HostDecision, len(hosts))
	if who.AgentUUID != "" || who.UserUUID == "" || len(hosts) == 0 {
		return out, nil
	}
	res := make([]Resource, 0, len(hosts))
	for _, h := range hosts {
		if h.Kind == KindDoc {
			continue // a doc never hosts a doc
		}
		res = append(res, Resource{Kind: h.Kind, UUID: h.UUID})
	}
	got := make(map[Resource]Decision, len(res))
	if err := resolveOthers(ctx, who, res, got); err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if d, ok := got[Resource{Kind: h.Kind, UUID: h.UUID}]; ok {
			out[h] = docBusiness.HostDecision{Level: d.Level, Was: d.Was, Deleted: d.Deleted, Archived: d.Archived}
		}
	}
	return out, nil
}
