package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	"github.com/google/uuid"
)

// levelRules gives the person one level on every doc.
type levelRules struct {
	accessBusiness.Resolver
	level accessBusiness.Level
}

func (l levelRules) Resolve(context.Context, accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error) {
	return accessBusiness.Decision{Level: l.level}, nil
}

// Publishing a doc's body as a template puts it in front of every member, so
// it needs the right to change the doc: someone who may only read or comment
// on a private doc can't publish it, and its body is never even read.
func TestOnlySomeoneWhoMayEditADocSavesItAsATemplate(t *testing.T) {
	saved, savedRead := accessBusiness.Rules, readStored
	defer func() { accessBusiness.Rules, readStored = saved, savedRead }()
	readStored = func(context.Context, string, string) (*dgraphStruct.DgraphDoc, error) {
		t.Fatal("the doc's body was read for someone who may not publish it")
		return nil, nil
	}
	for _, level := range []accessBusiness.Level{accessBusiness.LevelView, accessBusiness.LevelComment} {
		accessBusiness.Rules = levelRules{level: level}
		_, err := SaveAsTemplate(context.Background(), accessBusiness.Subject{}, uuid.New(), uuid.NewString(), "Leaked", "")
		if !errors.Is(err, ErrForbidden) {
			t.Fatalf("level %d: want ErrForbidden, got %v", level, err)
		}
	}
}
