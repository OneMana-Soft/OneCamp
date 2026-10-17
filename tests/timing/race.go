//go:build race

package timing

// Race says whether the race detector is built in. It slows a tight loop over
// bytes many times more than it slows other work (a parser's look for a
// closing bracket against its reading of prose), so a test that compares the
// times of different kinds of work asks only a plain build.
const Race = true
