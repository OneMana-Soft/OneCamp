# Optional: the deployment env file. `-include` (not `include`) so the quality
# targets — fmt, check_fmt, test, test_code_runner — work in a fresh checkout
# that has no .env yet. Targets that genuinely need it still pass --env-file and
# fail with their own clear error.
-include .env

install_pre_commit:
	pre-commit install

# ─── Code Quality ─────────────────────────────────────────────────────────────
fmt:
	gofmt -w $(shell find . -name '*.go' -not -path './data/*' -not -path './.next/*' -not -path './vendor/*')

check_fmt:
	gofmt -l $(shell find . -name '*.go' -not -path './data/*' -not -path './.next/*' -not -path './vendor/*')

install_githooks:
	git config core.hooksPath .githooks
	@echo "Git hooks installed. Pre-commit scans for secrets and auto-formats Go files."
# ─── Tests ────────────────────────────────────────────────────────────────────
# `make test` is the SAME gate CI runs, so a green local run means a green CI
# run. Two things to keep in mind before editing it:
#   * These are TREE patterns, not a package list, so a package added under any
#     of them is covered the day it lands. Do not narrow it to named packages —
#     a hand-list is how security-relevant suites silently stopped gating before.
#   * `./...` cannot be used from the repo root: it walks the runtime volumes in
#     data/ (root-owned, so discovery dies with "permission denied").
# Tests needing Postgres/Redis/OpenSearch/MinIO/Dgraph/MQTT sit behind the
# `integration` build tag and are excluded here. The KEKs mirror CI's throwaway
# test values and only apply when the environment hasn't already set them.
test:
	IMPORT_TOKEN_KEK="$${IMPORT_TOKEN_KEK:-make-test-kek-with-enough-entropy-1234567890}" \
	AI_CONFIG_KEK="$${AI_CONFIG_KEK:-make-test-ai-kek-with-enough-entropy-1234567890}" \
	go test -count=1 -race -timeout=20m -p=4 \
		./business/... ./models/... ./domain/... ./controllers/... ./services/... \
		./helpers/... ./middleware/... ./router/... ./adapter/... ./initializers/... \
		./docs/...
# ./docs is in that list because the guard living there checks that every file
# path the compliance documents cite actually exists, and a guard the gate does
# not run is a guard that does not exist. It was written, it passed when invoked
# by hand, and `make verify` reported OK on a branch where it fails. That is the
# same shape as the vacuous `go build ./...` this target was created to replace.

# The code-runner sidecar is a SEPARATE Go module, so the patterns above cannot
# see it. It executes untrusted repository code, which makes its containment
# tests worth running on their own. (Heavier suites need git + a toolchain and
# opt in via code_runner_coding_test.)
test_code_runner:
	cd other-services/code-runner && go test -count=1 -race -timeout=10m ./...
# Everything CI gates on, in one command.
test_all: test test_code_runner

# ─── verify ───────────────────────────────────────────────────────────────────
# What to run before pushing. Exists because the obvious command is a trap.
#
# `go build ./...` DOES NOT WORK in this checkout. Package traversal walks
# data/, hits a container-owned directory it cannot read, and fails with a
# permission error BEFORE compiling anything. Filter that line out and the
# output is empty, which reads exactly like success. It is not: nothing was
# compiled. The same applies to `go vet ./...` and `go test ./...` locally,
# though not in CI, where the data directory does not exist yet.
#
# So the package list is explicit, and cmd/server is built on its own because
# it is the binary that ships and every library below it can compile while the
# program does not link.
# Does the PRODUCT work on a running instance, as opposed to the services being up?
#
# The admin health page is read-only, which is what makes it safe to press and is
# why it cannot answer "can I create a task and then find it again". This can,
# because it WRITES: point it at a project you are happy to see test tasks in, or
# the writing steps skip themselves rather than run.
#
#   ONECAMP_BASE_URL=https://onecamp-backend.example.com \
#   ONECAMP_JOURNEY_TOKEN=oc_... \
#   ONECAMP_JOURNEY_PROJECT=<project uuid> \
#   make journey
journey:
	@go run ./cmd/journey

verify:
	@echo "▶ gofmt"
	@OUT="$$(gofmt -l $$(find . -name '*.go' -not -path './data/*' -not -path './.next/*' 2>/dev/null))"; \
		if [ -n "$$OUT" ]; then echo "These files need gofmt:"; echo "$$OUT"; exit 1; fi
	@echo "▶ build (the shipped binary first)"
	go build -o /dev/null ./cmd/server/
	go build ./business/... ./controllers/... ./domain/... ./models/... \
		./helpers/... ./router/... ./adapter/... ./initializers/... ./services/...
	@echo "▶ vet"
	go vet ./business/... ./controllers/... ./domain/... ./models/... \
		./helpers/... ./router/... ./adapter/... ./initializers/... ./services/... ./cmd/...
	@$(MAKE) --no-print-directory test
	@echo "▶ verify OK"

# ─── Individual services, all against final-compose.yml ───────────────────────
#
# These used to name a per-component compose file each -- postgres-compose.yml,
# redis-compose.yml, dgraph-compose.yml and seven more. Every one of them was a
# SECOND declaration of a service final-compose.yml already owns, and every one
# had drifted, because nothing ever compared them:
#
#   * Nine declared NO networks at all, so `up -d` would recreate the service on
#     the project's default bridge instead of onecamp-shared-net -- reachable by
#     nothing that needs it. For go-service (service-compose.yml) that also drops
#     traefik-public, so the API comes back healthy and Traefik cannot route to
#     it: the whole product down, with every container green.
#   * All but dgraph published ports to the host, turning internal-only
#     datastores into host-reachable ones. postgres, redis and minio among them.
#   * open-search-compose.yml was the worst. It declared opensearch-node1 -- a
#     DIFFERENT service name from final-compose.yml's opensearch-onecamp-node1 --
#     mounting THE SAME ./data/opensearch/opensearch-data1 directory. So it would
#     not have replaced the running node, it would have started a second
#     OpenSearch JVM on the live one's data directory.
#
# None of that is visible in review, and none of it fails until somebody runs a
# target named exactly after the thing they want to do, on a live host, in the
# middle of an incident. The files are deleted; two definitions of one service is
# the fault itself. Same conclusion as hocuspocus-compose.yml (388e956) and
# ollama-compose.yml (bb36c14) -- this is the third time, so it is now the rule.
#
# `stop`, not `down`. `down` against final-compose.yml stops and REMOVES every
# service in the file, so the old `stop_redis_containers` -- harmless when it
# named a file containing only redis -- would take the entire stack down here.
# `stop` is also what these target names actually promise.
#
# coturn-compose.yml and code-runner-coding-compose.yml survive: the first is the
# only definition of `turn` anywhere, and the second is a real overlay, applied
# on top of final-compose.yml rather than instead of it (see COMPOSE_RUNNERS).

create_postgres_container:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d postgres

stop_postgres_container:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop postgres

# exec, not up: this creates a database inside the container that is already
# running. It also used to hardcode `-p onecamp-beta`, so on any other stack it
# reached for a project that does not exist, or the wrong one.
create_postgres_db:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml exec postgres createdb --username=${DB_USER} --owner=${DB_USER} ${DB_NAME}

create_open_search_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d opensearch-onecamp-node1 opensearch-onecamp-dashboards

stop_open_search_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop opensearch-onecamp-node1 opensearch-onecamp-dashboards

create_redis_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d redis

stop_redis_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop redis

create_emqx_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d emqx

stop_emqx_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop emqx

create_dgraph_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d zero alpha ratel

stop_dgraph_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop zero alpha ratel

create_minio_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d minio

stop_minio_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop minio

create_cs_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d mongo-db ch-server otel-collector ch-app

down_cs_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop mongo-db ch-server otel-collector ch-app

create_livekit_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d livekit egress agent

stop_livekit_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop livekit egress agent

create_service:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d go-service

stop_service:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop go-service

# These name final-compose.yml, the definition the stack they act on actually
# uses. They used to name ollama-compose.yml, a separate declaration of the same
# two services with the same fixed container_name — which published 11434 to the
# host and, having no networks: block, joined the project's default network
# instead of onecamp-shared-net. Running the target named after starting Ollama
# would therefore have replaced a working, internal-only engine with one that
# go-service could not reach and the host could. That file is deleted; this is
# the same fault, and the same fix, as the hocuspocus-compose.yml stub in
# 388e956.
#
# `stop`, not `down`: down aimed at final-compose.yml would take the whole stack
# with it, and "stop" is what the target name promises anyway.
create_ollama_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d ollama ollama-init

stop_ollama_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop ollama ollama-init

# Move the inference engine to the version named by OLLAMA_IMAGE_TAG in .env.
#
# Two steps rather than one because the tag is pinned: edit OLLAMA_IMAGE_TAG,
# then run this. That is the whole update, and setting the old value back and
# running it again is the rollback. `pull` is explicit because `up -d` fetches
# only when the image is absent locally, which hides a re-pushed tag.
#
# Only ollama is recreated. Nothing else in the stack restarts, and models in
# ./data/ollama are a bind mount, so they survive untouched. In-flight AI
# requests fail for the seconds the engine is down; the admin panel reports the
# new version once it is healthy.
ollama_update:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml pull ollama
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d ollama

create_coturn_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f coturn-compose.yml up -d

stop_coturn_containers:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f coturn-compose.yml down



create_server_db:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml exec postgres createdb --username=${DB_USER} --owner=${DB_USER} ${DB_NAME}

migrate_up:
	sqlx migrate run --database-url "postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=disable"

migrate_down:
	sqlx migrate revert --database-url "postgres://${DB_USER}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}?sslmode=disable"

# Migrations on a SERVER. The docker network is RESOLVED, not hardcoded.
#
# These targets used to pass `--network onecamp`. The stack's network was renamed
# to onecamp-shared-net, and compose prefixes it with the project name
# (onecamp-beta_onecamp-shared-net), so `docker run` could not attach and the
# target failed. Migrations then quietly stopped being applied — which is how a
# binary needing migrations 133-137 came to be deployed against a schema at 132,
# leaving the server logging the same three "column does not exist" errors every
# five seconds while reporting itself healthy.
#
# Override the lookup with: make migrate_server_up ONECAMP_NET=<network>
#
# DERIVED FROM THE PROJECT, NOT GUESSED. This was `| head -1`, which takes whichever network docker
# happens to list first. More than one can match: final-compose.yml declares onecamp-shared-net
# without a name, so compose creates it prefixed (onecamp-beta_onecamp-shared-net), while a customer
# stack on the same host creates its own under its own project name. A host running beta beside any
# other stack therefore has at least two matches, and docker network ls is ordered by creation, not
# by relevance.
#
# What that costs depends on the host. Today the likely outcome is a failed connection, because a
# customer's postgres joins only its own `internal` network — nothing named postgres answers on the
# shared net. But two FULL stacks on one host (both using final-compose.yml) each put a postgres on
# their own prefixed shared net, and then head -1 silently runs these migrations against the other
# stack's database. Refusing to guess is cheap; that is not.
#
# The project name determines the prefix, so it determines the network. customer_migrate_up already
# works this way, deriving --network onecamp-$(CID)_internal from the CID rather than searching.
ONECAMP_NET_CANDIDATES = $(shell docker network ls --format '{{.Name}}' 2>/dev/null | grep -E '(^|_)onecamp-shared-net$$' | sort)

# Recursively expanded on purpose: this shells out to docker, and it must only do so when one of the
# migrate_server_* targets actually needs it — not on every `make test`.
#
# Resolution order: the network belonging to ONECAMP_PROJECT; failing that, the only candidate, so a
# single-stack host behaves exactly as before; failing that, nothing, and the guard below explains
# the ambiguity rather than picking.
ONECAMP_NET ?= $(strip $(if $(filter $(ONECAMP_PROJECT)_onecamp-shared-net,$(ONECAMP_NET_CANDIDATES)),\
	$(ONECAMP_PROJECT)_onecamp-shared-net,\
	$(if $(word 2,$(ONECAMP_NET_CANDIDATES)),,$(firstword $(ONECAMP_NET_CANDIDATES)))))

# require_onecamp_net refuses, with a different message for "none" and "several".
#
# The distinction is the whole point. "not found — is the stack up?" sent an operator to check a
# stack that was already running, when the real answer was that the lookup had two answers and no
# way to choose.
define require_onecamp_net
@if [ -n "$(ONECAMP_NET)" ]; then \
	echo "▶ docker network: $(ONECAMP_NET)"; \
else \
	candidates="$(ONECAMP_NET_CANDIDATES)"; \
	if [ -z "$$candidates" ]; then \
		echo "❌ No onecamp-shared-net network found — is the stack up?"; \
		echo "   Networks present:"; \
		docker network ls --format '{{.Name}}' | sed 's/^/     /'; \
		echo "   Override with: make $@ ONECAMP_NET=<network>"; \
	else \
		echo "❌ Ambiguous: several onecamp-shared-net networks exist, and none of them belongs to"; \
		echo "   compose project '$(ONECAMP_PROJECT)':"; \
		for n in $$candidates; do echo "     $$n"; done; \
		echo "   Refusing to guess. Picking the wrong one can run these migrations against another"; \
		echo "   stack's database."; \
		echo "   Name the stack:   make $@ ONECAMP_PROJECT=<compose project>"; \
		echo "   or the network:   make $@ ONECAMP_NET=<network>"; \
	fi; \
	exit 1; \
fi
endef

# ONECAMP_PROJECT is the docker compose project name. Defaulted to onecamp-beta so
# every existing target behaves exactly as before, but overridable — because a
# customer deployment uses onecamp-<CID>, and a name hardcoded in 33 places is the
# same trap as the hardcoded `--network onecamp` above: correct on one machine,
# silently wrong everywhere else. The compose project name also determines the
# network prefix, so the two belong together.
#
#   make migrate_server_status ONECAMP_PROJECT=onecamp-acme
ONECAMP_PROJECT ?= onecamp-beta

migrate_server_up:
	$(require_onecamp_net)
	docker run --rm -v $(shell pwd):/app -w /app --network $(ONECAMP_NET) rust:alpine sh -c "cargo install sqlx-cli --no-default-features --features postgres && sqlx migrate run --database-url postgres://${DB_USER}:${DB_PASSWORD}@postgres:5432/${DB_NAME}?sslmode=disable"

migrate_server_down:
	$(require_onecamp_net)
	docker run --rm -v $(shell pwd):/app -w /app --network $(ONECAMP_NET) rust:alpine sh -c "cargo install sqlx-cli --no-default-features --features postgres && sqlx migrate revert --database-url postgres://${DB_USER}:${DB_PASSWORD}@postgres:5432/${DB_NAME}?sslmode=disable"

# migrate_server_status shows what the DATABASE believes is applied, so a schema
# mismatch is diagnosable without reading application logs or guessing.
migrate_server_status:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml exec -T postgres \
		psql -U ${DB_USER} -d ${DB_NAME} -c "SELECT version, success, installed_on FROM _sqlx_migrations ORDER BY version DESC LIMIT 12;"

# ─── Analysis sandbox runner (opt-in) ───────────────────────────────────────────
# The code-runner sidecar behind the "code-execution" profile: the network-less
# sandbox agents use for bounded data analysis and chart rendering.
#
# WHY THESE TARGETS EXIST. The coding runner below had targets; this one had NONE.
# Its profile meant it never came up with a plain `up -d`, and with no documented
# way to start it, nobody did — a beta host ran for weeks with every other service
# healthy and this one simply absent, which presents as agents failing to analyse
# anything rather than as a missing container.
#
# BOTH COMPOSE FILES AND BOTH PROFILES ARE PASSED ON PURPOSE. Without them compose
# cannot see the coding-profile services and treats them as orphans, warning on
# every run and deleting them outright if anyone adds --remove-orphans. Naming the
# service explicitly keeps the blast radius to one container instead of
# reconciling all 22.
COMPOSE_RUNNERS := -f final-compose.yml -f code-runner-coding-compose.yml
COMPOSE_ALL_PROFILES := --profile code-execution --profile coding

code_runner_up:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) $(COMPOSE_RUNNERS) $(COMPOSE_ALL_PROFILES) up -d --build code-runner

code_runner_down:
	docker compose --project-name $(ONECAMP_PROJECT) $(COMPOSE_RUNNERS) $(COMPOSE_ALL_PROFILES) stop code-runner

# Reachability from the SERVICE THAT CALLS IT, not from the host. code-runner sits
# on code-runner-net with no published ports, so a host-side curl proves nothing:
# the only question that matters is whether go-service can reach it, and this asks
# exactly that. The admin UI still has to be pointed at it
# (Admin -> AI Models -> Code analysis sandbox, http://code-runner:9099/run).
code_runner_status:
	@docker compose --env-file .env --project-name $(ONECAMP_PROJECT) $(COMPOSE_RUNNERS) $(COMPOSE_ALL_PROFILES) ps code-runner
	@echo "▶ reachability from go-service:"
	@docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml exec -T go-service \
		sh -c 'wget -qO- --timeout=5 http://code-runner:9099/healthz && echo "" || echo "UNREACHABLE from go-service"'

# ─── Code-PR coding runner (opt-in) ─────────────────────────────────────────────
# Bring up the hardened coding runner + its allowlist egress proxy (profile
# "coding"), prove containment, and run the (git + toolchain) integration suite.
# The two services are named for the same reason code_runner_up names its one:
# without them `up --build` rebuilds and recreates every service in both files,
# the API and databases included, to start a runner.
code_runner_coding_up:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml -f code-runner-coding-compose.yml --profile coding up -d --build egress-proxy code-runner-coding

code_runner_coding_down:
	docker compose --project-name $(ONECAMP_PROJECT) -f final-compose.yml -f code-runner-coding-compose.yml --profile coding down

# Prove the runner is CONTAINED (egress allowlist + no direct internet) before
# enabling code PRs. Run after code_runner_coding_up. Reloads the egress proxy
# first so an edited allowlist (egress-proxy/filter-allowlist.txt) always takes
# effect — tinyproxy only re-reads its filter on restart.
code_runner_coding_smoke:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml -f code-runner-coding-compose.yml --profile coding restart egress-proxy
	./scripts/code-runner-coding-smoke.sh

# The handler's own safety invariants (push-never-to-base/force, no token in
# .git/config, edit/verify loop) — needs git + the go toolchain.
code_runner_coding_test:
	cd other-services/code-runner && go test -tags coderun_integration ./...

# --pull, NOT --no-cache.
#
# --no-cache was here to stop a stale layer shipping the previous commit. It cannot:
# COPY is content-addressed, so any change to the files copied invalidates that layer
# and everything after it. What --no-cache actually did was discard the dependency
# download on every build and write a full set of layers nothing would reuse — the
# build cache reached 32GB of never-reused records that way.
#
# The freshness that IS worth buying is the base image, and --no-cache never bought
# it: re-resolving golang:1.26-alpine is what --pull does.
build_image:
	docker build --pull -t go_service_onecamp:1.0 .



create_all_service:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d

stop_all_service:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml down

# ─── Deploy new backend code to THIS stack ────────────────────────────────────
# The most frequent operation on a running deployment, and it had no target at all — only
# customer_rebuild_go, for customer stacks. So deploying to beta or prod meant typing a compose
# invocation from memory, which is how the collaboration-service targets below came to be pointed at
# the wrong file for a year.
#
# BUILDS THE IMAGE FIRST, and that is the whole point rather than a convenience. final-compose.yml
# declares `image: go_service_onecamp:1.0` with NO build section, so `up -d --force-recreate go-service`
# on its own recreates the container from the image already on disk: the old binary, restarted. The
# operator sees a container come up healthy and believes they deployed. customer_rebuild_go has exactly
# that bug and is fixed below.
#
# build_image pulls a fresh base image and reuses the dependency layer. A cached layer cannot ship
# the previous commit: COPY is content-addressed, so changed source invalidates it and everything after.
rebuild_go: build_image
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d --force-recreate go-service
	@echo "▶ deployed. Follow the boot with: make go_logs"

# Needed more than it looks since 407b6fe: the KEK probes EXIT the process when a key is a template
# placeholder, so a misconfigured deploy presents as a container that will not stay up, and the reason
# is only in these logs.
go_logs:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml logs -f --tail=200 go-service

# ─── Self-hosted transcription (optional) ─────────────────────────────────────
# The `transcription` compose profile. Opt-in because it downloads a model and
# holds it in memory; nothing else in the stack changes when it is off.
#
# After `make stt_up`, choose "Self-hosted (bundled)" in Admin > Transcription
# and press Test. The first request after a cold start is slow: the model is
# being fetched.
stt_up:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml --profile transcription up -d whisper
	@echo "▶ transcription server starting. It downloads its model on first use; watch with: make stt_logs"

stt_down:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml --profile transcription stop whisper

stt_logs:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml --profile transcription logs -f --tail=200 whisper

ps:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml ps

logs:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml logs -f --tail=100

build_livekit_agent:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d --build agent

# ─── Collaboration service (docs + boards realtime) ───────────────────────────
# THESE USED TO POINT AT hocuspocus-compose.yml, WHICH WAS A DIFFERENT SERVICE.
#
# That file was a 2025 local-dev stub declaring collaboration-service with
# GO_BACKEND_URL=host.docker.internal, no REDIS_PASSWORD, no traefik labels and no restart policy —
# while the deployed definition in final-compose.yml has all four. Run against a real stack with the
# same --project-name, `up -d --build collaboration-service` therefore REPLACED the working container
# with one that cannot authenticate to Redis, is unreachable over WSS, and does not come back after a
# crash. The last of those would have silently undone the restart policy added in b91fc42, whose whole
# point was that a single unhandled rejection had left docs and boards showing "Reconnecting…" for two
# weeks. The target named after deploying this service was the thing that broke it.
#
# The stub is deleted rather than kept beside the real definition, because two definitions of one
# service is the fault itself: nothing referenced it except these three targets, and it had drifted for
# over a year without anyone noticing.
collab_up:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml up -d --build collaboration-service

# STOPS rather than removes, matching code_runner_down. `down` on a subset of a running project prints
# orphan warnings for the other 24 services and deletes them outright if anyone adds --remove-orphans.
collab_down:
	docker compose --project-name $(ONECAMP_PROJECT) -f final-compose.yml stop collaboration-service

collab_logs:
	docker compose --env-file .env --project-name $(ONECAMP_PROJECT) -f final-compose.yml logs -f --tail=200 collaboration-service

build:
	if [ -f "${BINARY}" ]; then \
		rm ${BINARY}; \
		echo "Deleted ${BINARY}"; \
	fi
	@echo "Building binary ..."
	go build -o ${BINARY} cmd/server/*.go

run: build
	@echo "Running Build..."
	./${BINARY}

stop:
	@echo "stopping server..."
	@-pkill -SIGTERM -f "./${BINARY}"
	@echo "server stopped"

# ═══════════════════════════════════════════════════════════════════════════════
# Multi-Tenant Customer Commands — GONE
# ═══════════════════════════════════════════════════════════════════════════════
# This file is the DEVELOPMENT Makefile: it drives final-compose.yml, the stack
# on the beta and demo host. What a customer runs is Makefile-distribute, which
# ships inside the zip beside sample-compose.yml, and that is also what cloud
# provisioning drives over SSH.
#
# The per-tenant targets that used to live below, and the shared-services block
# above them, described one host running several customers at once. That
# topology is gone and so are both of its compose files.
# ═══════════════════════════════════════════════════════════════════════════════

# ─── Shared Services ──────────────────────────────────────────────────────────
# REMOVED, along with shared-compose.yml and shared.env. That stack existed to
# run one LiveKit and one Ollama for several tenants back when OneCamp cloud was
# hosted beside beta on the same box. Nothing has run it since, and a set of
# targets nobody runs is a set of targets nobody notices going stale: shared.env
# still held a live Redis password and a live LiveKit secret, in git, for a
# stack that no longer exists.
#
# Ollama on a single host is ollama_update.

# ─── Customer Services ────────────────────────────────────────────────────────
# REMOVED, along with customer-compose.yml and scripts/export-customer-data.sh.
#
# Thirteen targets and a compose file for running one stack per customer on a
# shared box, keyed by CID. Nothing reached them. What a customer gets is the
# zip, which ships distribute-compose.yml as sample-compose.yml and is driven by
# Makefile-distribute; cloud provisioning runs that same file with
# --project-name onecamp, one workspace per box, through `make install` and
# `make update`. So this was the second half of the multi-tenant-on-one-host
# topology whose other half, shared-compose.yml, was deleted earlier for the
# same reason: a set of targets nobody runs is a set of targets nobody notices
# going stale, and the last one to go stale had a live Redis password in it.
