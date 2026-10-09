package helpers

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestParseMqttUsername(t *testing.T) {
	id := uuid.New()
	if got, ok := ParseMqttUsername("user_" + id.String()); !ok || got != id {
		t.Fatalf("a person's username: %v %v", got, ok)
	}
	for _, name := range []string{
		"backend", // the backend's own processes: used to crash the $SYS handler
		"dashboard",
		"",
		"user_",
		"user",
		"user_" + id.String() + "_x",
		"user_x_" + id.String(),
		"x_" + id.String(),
		"USER_" + id.String(),
		"user_" + id.String()[:35],
		// The same id written another way is not how the backend writes it.
		"user_{" + id.String() + "}",
		"user_urn:uuid:" + id.String(),
		"user_" + strings.ReplaceAll(id.String(), "-", ""),
		"user_" + strings.ToUpper(id.String()),
	} {
		if got, ok := ParseMqttUsername(name); ok {
			t.Errorf("ParseMqttUsername(%q) = %v, a person", name, got)
		}
	}
}
