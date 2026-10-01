package helpers

// IsClientSubcommand reports whether a process was started as the named
// subcommand rather than as the server.
//
// WHY A PURE FUNCTION OF ITS ARGUMENTS. The decision it makes is asymmetric and
// unforgiving: wrong in one direction the binary runs a diagnostic instead of
// starting the server, and the product is down on the next restart; wrong in the
// other it boots a full server, workers and all, beside the instance it was
// asked to check. Taking args as a parameter is what lets that be tested at all,
// because the alternative reads os.Args inside a package whose init() insists on
// a database configuration before any test can run.
//
// The match is exact and positional. "journeys" is not "journey", and a
// subcommand appearing anywhere other than immediately after the program name is
// somebody else's argument.
func IsClientSubcommand(args []string, name string) bool {
	if name == "" || len(args) < 2 {
		return false
	}
	return args[1] == name
}

// OneShotSubcommands run once against this server's own stores and exit. They
// boot the stores the server uses, but they are not a server, and must never
// run its background loops: `demoseed` runs every night beside the live
// instance, and a worker in that short-lived process could claim an agent's
// job, exit mid-run, and leave it to be reclaimed and posted twice.
var OneShotSubcommands = []string{"demoseed"}

// RoleForProcess is the role this process should actually take: a one-shot
// subcommand runs no workers, whatever SERVICE_ROLE says; anything else keeps
// its configured role. Pure.
func RoleForProcess(role ServiceRole, args []string) ServiceRole {
	for _, name := range OneShotSubcommands {
		if IsClientSubcommand(args, name) {
			return RoleAPI
		}
	}
	return role
}
