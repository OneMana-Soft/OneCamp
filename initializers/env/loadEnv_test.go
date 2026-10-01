package env

import (
	"os"
	"testing"
)

// The guard that decides whether a missing .env is fatal.
//
// It became load-bearing when the image stopped baking .env in: a container gets
// its configuration through env_file, so insisting on a file crash-looped every
// deployment. It must still fail for a developer who has not copied .sample.env,
// because "connection refused" fifty lines later is a far worse message.
func TestConfiguredFromEnvironment(t *testing.T) {
	clear := func() {
		t.Helper()
		for _, k := range []string{"DB_NAME", "DB_HOST"} {
			orig, had := os.LookupEnv(k)
			os.Unsetenv(k)
			t.Cleanup(func() {
				if had {
					os.Setenv(k, orig)
				}
			})
		}
	}

	t.Run("empty environment is not configured", func(t *testing.T) {
		clear()
		if ConfiguredFromEnvironment() {
			t.Fatal("an empty environment reported itself as configured, so a developer\n" +
				"who forgot to create .env would get a database error instead of the reason")
		}
	})

	t.Run("either key alone is enough", func(t *testing.T) {
		for _, k := range []string{"DB_NAME", "DB_HOST"} {
			clear()
			os.Setenv(k, "x")
			if !ConfiguredFromEnvironment() {
				t.Fatalf("%s was set and it still demanded a .env file, which is the\n"+
					"crash loop this exists to prevent", k)
			}
			os.Unsetenv(k)
		}
	})
}
