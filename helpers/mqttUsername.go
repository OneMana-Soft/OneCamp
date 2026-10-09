package helpers

import (
	"strings"

	"github.com/google/uuid"
)

// ParseMqttUsername is the person a broker username names. A person's client
// signs in as "user_" and their id, which the backend writes into the
// client's broker token; that, written exactly as the backend writes it, is
// the only form that names anyone. Anything else (the backend's own
// processes, "backend"; the dashboard; a name the backend never writes) names
// nobody.
func ParseMqttUsername(username string) (uuid.UUID, bool) {
	idText, ok := strings.CutPrefix(username, "user_")
	if !ok {
		return uuid.Nil, false
	}
	id, err := uuid.Parse(idText)
	if err != nil || id.String() != idText {
		return uuid.Nil, false
	}
	return id, true
}
