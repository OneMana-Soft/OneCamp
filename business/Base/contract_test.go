package business

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Every table is in the one base until bases exist.
func TestEveryTableIsInTheOneBase(t *testing.T) {
	for i := 0; i < 3; i++ {
		got, err := Bases.BaseOf(context.Background(), uuid.New())
		if err != nil || got != DefaultBase {
			t.Errorf("base %v, %v", got, err)
		}
	}
	if DefaultBase.Version() != 4 || DefaultBase.Variant() != uuid.RFC4122 {
		t.Errorf("the default base's id %v isn't a version 4 uuid like every other id", DefaultBase)
	}
}
