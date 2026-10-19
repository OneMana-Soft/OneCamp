package business

import (
	"context"
	"testing"

	docBusiness "github.com/akashc777/OneCamp/business/Doc"
)

// probeHostRule decides the probe kind's hosts: full for an admin, edit for
// anyone else, and remembers who it was asked for.
type probeHostRule struct{ asked *[]Subject }

func (p probeHostRule) DecideMany(_ context.Context, who Subject, ids []string) (map[string]Decision, error) {
	*p.asked = append(*p.asked, who)
	out := map[string]Decision{}
	for _, id := range ids {
		d := Decision{Level: LevelEdit, Was: LevelEdit}
		if who.Admin {
			d = Decision{Level: LevelFull, Was: LevelFull}
		}
		out[id] = d
	}
	return out, nil
}

// A hosted doc's host is decided for the person the doc's decision is for,
// as the resolver was asked: its subject, a token's cleared admin flag and an
// agent among it, rides the context. An agent opens no hosted doc; a caller
// that opened hosted docs without the resolver decides for the person alone,
// with no admin power; and a doc never hosts a doc.
func TestHostsAreDecidedForTheResolversSubject(t *testing.T) {
	var asked []Subject
	RegisterKind("probe_host_row", probeHostRule{asked: &asked})
	defer func() {
		kindsMu.Lock()
		delete(kinds, "probe_host_row")
		kindsMu.Unlock()
	}()
	row := docBusiness.Host{Kind: "probe_host_row", UUID: "8e2b4d6f-1a3c-4e5f-8b7a-9c0d1e2f3a4b"}
	v := docBusiness.Viewer{UID: "0x9", UUID: "99999999-9999-4999-8999-999999999999"}

	admin := Subject{UserUID: v.UID, UserUUID: v.UUID, Admin: true}
	got, err := decideHosts(withSubject(context.Background(), admin), v, []docBusiness.Host{row})
	if err != nil || got[row].Level != LevelFull || len(asked) != 1 || !asked[0].Admin {
		t.Fatalf("the resolver's admin subject: %+v, %v, asked %+v", got, err, asked)
	}

	// The resolver asked through a token: its admin flag is cleared, and stays so.
	got, _ = decideHosts(withSubject(context.Background(), Subject{UserUID: v.UID, UserUUID: v.UUID}), v, []docBusiness.Host{row})
	if got[row].Level != LevelEdit || asked[1].Admin {
		t.Errorf("a token's subject: %+v, asked %+v", got, asked[1])
	}

	agent := Subject{UserUID: v.UID, UserUUID: v.UUID, Admin: true, AgentUUID: "77777777-7777-4777-8777-777777777777"}
	got, _ = decideHosts(withSubject(context.Background(), agent), v, []docBusiness.Host{row})
	if len(got) != 0 || len(asked) != 2 {
		t.Errorf("an agent opened a hosted doc: %+v (asked %d)", got, len(asked))
	}

	// Someone else's subject on the context isn't the viewer's.
	got, _ = decideHosts(withSubject(context.Background(), Subject{UserUUID: "11111111-1111-4111-8111-111111111111", Admin: true}), v, []docBusiness.Host{row})
	if got[row].Level != LevelEdit || asked[2].Admin || asked[2].UserUUID != v.UUID {
		t.Errorf("another person's subject decided the viewer's host: %+v, asked %+v", got, asked[2])
	}

	// No subject: the person alone, no admin.
	got, _ = decideHosts(context.Background(), v, []docBusiness.Host{row})
	if got[row].Level != LevelEdit || asked[3].Admin {
		t.Errorf("no subject: %+v, asked %+v", got, asked[3])
	}

	got, err = decideHosts(context.Background(), v, []docBusiness.Host{{Kind: KindDoc, UUID: row.UUID}})
	if err != nil || len(got) != 0 {
		t.Errorf("a doc hosted a doc: %+v, %v", got, err)
	}
}
