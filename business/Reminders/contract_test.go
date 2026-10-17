package business

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// Today nobody is reminded: no defaults, and a reminder isn't taken that
// nothing would fire.
func TestNobodyIsRemindedToday(t *testing.T) {
	if got, err := Reminders.Defaults(context.Background(), "cal"); err != nil || len(got) != 0 {
		t.Errorf("defaults: %v %v", got, err)
	}
	if err := Reminders.Set(context.Background(), uuid.New(), "p", []Rule{{MinutesBefore: 10, Method: "push"}}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("set: %v", err)
	}
}
