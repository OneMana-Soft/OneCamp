package helpers

// SubcommandKind is how a subcommand runs.
type SubcommandKind int

const (
	// ClientSubcommand speaks HTTP to a running instance, as any client does:
	// it connects to none of this server's stores, starts nothing and binds no
	// port (`journey`).
	ClientSubcommand SubcommandKind = iota + 1
	// OneShotSubcommand runs once against this server's own stores and exits.
	// It connects their clients and nothing else (RoleOneShot): no loop, no
	// consumer, no seed, no port. `demoseed` runs every night beside the live
	// instance, and a worker in that short-lived process could claim an
	// agent's job, exit mid-run, and leave it to be reclaimed and posted twice;
	// `calendarmove` started the email, workflow and agent workers beside the
	// server it was moving (17 Oct 2026).
	OneShotSubcommand
)

// Subcommands is every subcommand this binary answers to, and how it runs. A
// new one is added here, and main runs it the way its kind says: one-shots
// through runOneShot (cmd/server/oneshot.go), the one path that connects the
// stores and starts nothing. oneShotGuard_test.go holds main to it.
var Subcommands = map[string]SubcommandKind{
	"journey":      ClientSubcommand,
	"demoseed":     OneShotSubcommand,
	"repairdocs":   OneShotSubcommand,
	"calendarmove": OneShotSubcommand,
}

// SubcommandOf is the subcommand args names and its kind, or "" and 0 for the
// server itself. Pure.
//
// WHY A PURE FUNCTION OF ITS ARGUMENTS. The decision it makes is asymmetric and
// unforgiving: wrong in one direction the binary runs a command instead of
// starting the server, and the product is down on the next restart; wrong in
// the other it boots a full server, workers and all, beside the instance it
// was asked to check or change. Taking args as a parameter is what lets that
// be tested at all, because the alternative reads os.Args inside a package
// whose init() insists on a database configuration before any test can run.
//
// The match is exact and positional. "journeys" is not "journey", and a
// subcommand appearing anywhere other than immediately after the program name
// is somebody else's argument.
func SubcommandOf(args []string) (string, SubcommandKind) {
	if len(args) < 2 {
		return "", 0
	}
	if kind, ok := Subcommands[args[1]]; ok {
		return args[1], kind
	}
	return "", 0
}

// OneShotOf is the one-shot subcommand args names, if it names one. Pure.
func OneShotOf(args []string) (string, bool) {
	name, kind := SubcommandOf(args)
	return name, kind == OneShotSubcommand
}

// RoleForProcess is the role this process should actually take: a one-shot
// subcommand is RoleOneShot, whatever SERVICE_ROLE says; anything else keeps
// its configured role. Pure.
func RoleForProcess(role ServiceRole, args []string) ServiceRole {
	if _, ok := OneShotOf(args); ok {
		return RoleOneShot
	}
	return role
}
