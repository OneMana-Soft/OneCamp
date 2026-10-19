package helpers

import "testing"

// The one store is the default (CAL-DEFAULT, v2.74.0): an install that sets
// nothing moves into it, and only CALENDAR_STORE=graph keeps the old stores.
func TestTheCalendarsOneStoreIsTheDefault(t *testing.T) {
	for value, want := range map[string]bool{
		"": true, "postgres": true, "Postgres ": true, "graph": false, " GRAPH\n": false, "postgress": true,
	} {
		t.Setenv(CalendarStoreEnv, value)
		if got := CalendarInPostgres(); got != want {
			t.Errorf("CALENDAR_STORE=%q: one store %v, want %v", value, got, want)
		}
	}
}
