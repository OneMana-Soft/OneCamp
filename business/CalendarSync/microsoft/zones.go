package microsoft

import (
	"strings"
	"time"

	ICS "github.com/akashc777/OneCamp/business/ICS"
)

// Outlook names zones by their Windows names; OneCamp by IANA's. The pairs
// are the codec's one table (business/ICS windowsZones.go: CLDR's
// windowsZones.xml territory "001", each Windows zone's main IANA zone),
// which also reads the TZIDs Outlook writes in a file.

// ianaAliases are IANA names CLDR keeps that Go knows by another (or the
// reverse), read as the one the table holds.
var ianaAliases = map[string]string{
	"Asia/Calcutta": "Asia/Kolkata", "Asia/Katmandu": "Asia/Kathmandu", "Asia/Saigon": "Asia/Bangkok", "Europe/Kyiv": "Europe/Kiev",
	"America/Indiana/Indianapolis": "America/Indianapolis", "America/Nuuk": "America/Godthab", "Asia/Yangon": "Asia/Rangoon",
	"America/Argentina/Buenos_Aires": "America/Buenos_Aires", "Etc/UTC": "UTC", "Etc/GMT": "UTC", "GMT": "UTC", "Etc/Universal": "UTC",
}

var ianaToWindows = func() map[string]string {
	out := map[string]string{"UTC": "UTC"}
	ICS.EachWindowsZone(func(w, z string) {
		if z != "Etc/UTC" {
			out[z] = w
		}
	})
	return out
}()

// ianaZone is a zone Outlook names, as IANA names it: a Windows name by
// CLDR's table, an IANA name as it is, and anything else ("tzone://Microsoft/
// Custom", a name nobody knows) as no zone. Pure.
func ianaZone(name string) string {
	name = strings.TrimSpace(name)
	switch name {
	case "":
		return ""
	case "UTC", "Coordinated Universal Time", "Etc/UTC", "GMT":
		return "UTC"
	}
	if z, ok := ICS.WindowsZoneOf(name); ok {
		return z
	}
	if a, ok := ianaAliases[name]; ok {
		name = a
	}
	if strings.Contains(name, "/") && !strings.Contains(name, "://") {
		if _, err := time.LoadLocation(name); err == nil {
			return name
		}
	}
	return ""
}

// windowsZone is an IANA zone as Outlook names it, "UTC" for none, and ""
// for a zone Outlook has no name for (sent in UTC instead). Pure.
func windowsZone(iana string) string {
	if iana == "" {
		return "UTC"
	}
	if a, ok := ianaAliases[iana]; ok {
		iana = a
	}
	return ianaToWindows[iana]
}

// location is a zone to read and write times in, UTC for none. Pure.
func location(zone string) *time.Location {
	if zone == "" {
		return time.UTC
	}
	if l, err := time.LoadLocation(zone); err == nil {
		return l
	}
	return time.UTC
}
