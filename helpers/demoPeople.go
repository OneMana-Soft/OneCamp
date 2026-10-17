package helpers

import "strings"

// The public demo's real members are the people who run it, signed up with
// their own names: on the demo, the shared visitor's @mention popup, the
// people it could message, add to a channel or a project, and the people its
// searches found, all listed them beside the seeded cast ("cannabisd",
// "Aditya", "Akash"). ServeHidingEmails blanks their addresses, but their
// names and faces are theirs too, and the visitor is anyone at all.
//
// So every list the visitor picks people from holds only the demo's own
// people: the visitor, the seeded cast (business/DemoSeed.Cast, all under
// DemoCastEmailDomain) and bot principals (agents, the assistant, the guest
// and Slack relays), which are the demo's by construction. Like
// ServeHidingEmails it is keyed on the visitor alone: the people who run the
// demo, signed in as themselves, and every member of every other server see
// every list as it always was.

// DemoCastEmailDomain is the domain every member of the demo's seeded cast
// has an address under. Reserved for the demo and never delivered to.
const DemoCastEmailDomain = "@cast.demo.onemana.dev"

// BotEmailDomain is the reserved domain every bot principal's email sits
// under (domain/User names it too). Not a real mail domain.
const BotEmailDomain = "@bot.onecamp.local"

// IsDemoPerson reports whether email belongs to one of the demo's own people:
// the shared visitor, a member of the seeded cast, or a bot principal. Pure
// apart from reading who the visitor is.
func IsDemoPerson(email string) bool {
	e := NormalizeEmail(email)
	if e == "" {
		return false
	}
	return strings.HasSuffix(e, DemoCastEmailDomain) || strings.HasSuffix(e, BotEmailDomain) || IsDemoVisitor(e)
}

// DemoListsPerson reports whether a list of people shown to viewerEmail may
// hold the person with personEmail (isBot: the person is a bot principal).
// Always true unless the viewer is the demo's shared visitor; then only the
// demo's own people (IsDemoPerson) and bots. A person with no address known
// is left out: on the demo, unknown is not the demo's.
func DemoListsPerson(viewerEmail, personEmail string, isBot bool) bool {
	if !IsDemoVisitor(viewerEmail) {
		return true
	}
	return isBot || IsDemoPerson(personEmail)
}

// DemoPeopleOnly returns people without those DemoListsPerson leaves out of a
// list shown to viewerEmail, reading each one's address and bot flag with
// email and isBot. For anyone but the demo's visitor, people comes back as it
// is; otherwise a new slice, so a cached list is never changed.
func DemoPeopleOnly[T any](viewerEmail string, people []T, email func(T) string, isBot func(T) bool) []T {
	if !IsDemoVisitor(viewerEmail) {
		return people
	}
	kept := make([]T, 0, len(people))
	for _, p := range people {
		if DemoListsPerson(viewerEmail, email(p), isBot(p)) {
			kept = append(kept, p)
		}
	}
	return kept
}
