package env

import (
	"log"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// UPLOAD_LIMIT_IN_MB is the legacy boot fallback for the per-file upload limit.
// The authoritative value now lives in the DB (business/Settings, admin-tunable
// with this env var as fallback), so an unset/invalid value here is no longer
// fatal — it simply defaults to 10 MB and the settings layer takes over.
var UPLOAD_LIMIT_IN_MB int64 = 10

// ConfiguredFromEnvironment reports whether configuration is already present in
// the process environment, so a missing .env file is not a problem.
//
// Split out from LoadEnvVariables because that one ends in log.Fatal, and a
// decision worth getting right should not need a subprocess to exercise. These two
// keys are chosen because nothing runs without a database: they are set in every
// compose file and in .sample.env, and neither has ever been optional.
func ConfiguredFromEnvironment() bool {
	return os.Getenv("DB_NAME") != "" || os.Getenv("DB_HOST") != ""
}

// LoadEnvVariables reads .env when there is one, and does not insist on it.
//
// A MISSING .env IS NOT AN ERROR IN A CONTAINER. Compose passes configuration
// through env_file, which puts it in the process environment — the file itself
// never needs to exist inside the image. Requiring one was survivable only because
// `COPY . .` happened to bake the deployment's .env into the image, which is also
// how the deployment's secrets ended up inside a distributable artefact. Removing
// that turned this line into a crash loop, which is how it was found.
//
// It stays fatal when the environment is empty too, because that is the case it
// was written for: a developer who has not copied .sample.env to .env yet, and for
// whom "connection refused" fifty lines later is a much worse message.
func LoadEnvVariables() {
	if err := godotenv.Load(); err != nil {
		if !ConfiguredFromEnvironment() {
			log.Fatal("Error loading .env file, and no configuration in the environment either. " +
				"Locally: cp ./.sample.env ./.env. In a container: pass env_file or environment.")
		}
		log.Printf("env: no .env file; using the configuration already in the environment")
	}
	applyOptionalOverrides()
}

// LoadEnvVariablesOptional reads .env when there is one and NEVER exits.
//
// For subcommands that talk to a remote instance over HTTP rather than being the
// server. They need none of this process's database configuration, and the fatal
// above would stop the journey check running from a laptop or a CI job -- which
// is exactly where a check against a deployed instance belongs. A .env is still
// read when present, so a developer's local settings are picked up either way.
func LoadEnvVariablesOptional() {
	if err := godotenv.Load(); err != nil {
		log.Printf("env: no .env file; using the configuration already in the environment")
	}
	applyOptionalOverrides()
}

func applyOptionalOverrides() {
	if v := os.Getenv("UPLOAD_LIMIT_IN_MB"); v != "" {
		if n, perr := strconv.ParseInt(v, 10, 64); perr == nil && n > 0 {
			UPLOAD_LIMIT_IN_MB = n
		} else {
			log.Printf("env: invalid UPLOAD_LIMIT_IN_MB=%q, using default %d MB", v, UPLOAD_LIMIT_IN_MB)
		}
	}
}
