#!/bin/bash
# code-runner-coding-smoke.sh — prove the CODE-PR coding runner is CONTAINED
# before enabling the feature. Run AFTER bringing the coding profile up:
#
#   docker compose -f final-compose.yml -f code-runner-coding-compose.yml \
#     --profile coding up -d
#   ./scripts/code-runner-coding-smoke.sh
#
# It asserts the three containment invariants that a Go test can't (they need
# the real network topology):
#   1. Liveness: the coding runner answers /healthz on the internal network.
#   2. Egress allowlist: through the proxy, the GIT HOST is reachable but a
#      NON-allowlisted host is REFUSED (default-deny works).
#   3. No direct internet: without the proxy, the runner's network has no route
#      out (so untrusted code can't call home).
#   4. The runner can actually USE that route: git speaks to the git host through
#      the proxy, and the runner container carries the proxy variables.
#
# Checks 1–3 prove CONTAINMENT. Check 4 exists because containment alone was not
# enough: the runner builds a deliberately secret-free environment for its child
# processes, which also stripped HTTP(S)_PROXY — the only route out — so every
# clone and push failed while checks 1–3 kept passing, because they exercise the
# proxy from their own throwaway container instead of through the runner. Proving
# the guard works is not the same as proving the caller can get through it.
#
# Exit non-zero on any failure so this can gate a deploy. Uses a throwaway
# curl container on the SAME internal network + proxy env as the runner, so it
# mirrors exactly what executing code could reach.
set -euo pipefail

COMPOSE=(docker compose -f final-compose.yml -f code-runner-coding-compose.yml)
NET="code-runner-net"
PROXY="http://egress-proxy:8888"
GIT_HOST="${GIT_HOST:-https://github.com}"
BLOCKED_HOST="${BLOCKED_HOST:-https://example.com}"

# Resolve the actual docker network name (compose prefixes with the project).
NET_NAME="$(docker network ls --format '{{.Name}}' | grep -E "(^|_)${NET}$" | head -1 || true)"
if [ -z "$NET_NAME" ]; then
  echo "❌ could not find the ${NET} docker network — is the coding profile up?"
  exit 1
fi
echo "▶ using network: ${NET_NAME}"

run_curl() { # args: <proxy-or-noproxy> <url> ; prints HTTP behavior, returns curl exit
  local mode="$1" url="$2"
  if [ "$mode" = "proxy" ]; then
    docker run --rm --network "$NET_NAME" -e https_proxy="$PROXY" -e http_proxy="$PROXY" \
      curlimages/curl:latest -sS -o /dev/null -w '%{http_code}' --max-time 15 "$url"
  else
    docker run --rm --network "$NET_NAME" \
      curlimages/curl:latest -sS -o /dev/null -w '%{http_code}' --max-time 15 --noproxy '*' "$url"
  fi
}

fail=0

# 1. Liveness.
echo "▶ [1/4] coding runner /healthz…"
if docker run --rm --network "$NET_NAME" curlimages/curl:latest \
     -sS --max-time 10 http://code-runner-coding:9099/healthz | grep -q ok; then
  echo "  ✅ runner is live"
else
  echo "  ❌ runner did not answer /healthz"; fail=1
fi

# 2a. Allowlisted git host reachable THROUGH the proxy. This ALSO proves the
# proxy is actually alive — critical, because a DOWN proxy makes every request
# return 000, which would otherwise masquerade as "everything blocked" and give
# a false sense of containment. So if the allowed host can't get through, treat
# the proxy as broken and skip the deny checks (they'd be meaningless).
echo "▶ [2/4] egress allowlist…"
git_code="$(run_curl proxy "$GIT_HOST" 2>/dev/null || true)"
if [ "$git_code" != "000" ] && [ -n "$git_code" ]; then
  echo "  ✅ git host reachable via proxy (HTTP ${git_code})"

  # 2b. Non-allowlisted host MUST be refused by the proxy. Only meaningful now
  #     that we know the proxy is up (allowed host succeeded above).
  blk_code="$(run_curl proxy "$BLOCKED_HOST" 2>/dev/null || true)"
  if [ "$blk_code" = "403" ] || [ "$blk_code" = "000" ] || [ -z "$blk_code" ]; then
    echo "  ✅ non-allowlisted host blocked by proxy (HTTP ${blk_code:-refused})"
  else
    echo "  ❌ non-allowlisted host was REACHABLE via proxy (HTTP ${blk_code}) — allowlist is not enforcing!"; fail=1
  fi
else
  echo "  ❌ git host NOT reachable via the proxy."
  echo "     The proxy is likely DOWN or misconfigured (a dead proxy returns 000"
  echo "     for everything and can look like 'blocked' — it is NOT containment)."
  echo "     Check it:  docker logs \$(docker ps -aqf name=egress-proxy) | tail -40"
  echo "     (skipping the deny checks — they are meaningless while the proxy is down)"
  fail=1
fi

# 3. No DIRECT internet (bypassing the proxy) from the runner's network.
echo "▶ [3/4] no direct internet…"
if code="$(run_curl noproxy "$BLOCKED_HOST" 2>/dev/null || true)"; then
  if [ "$code" = "000" ] || [ -z "$code" ]; then
    echo "  ✅ no direct internet route (as expected)"
  else
    echo "  ❌ the runner network has DIRECT internet (HTTP ${code}) — it must be internal-only!"; fail=1
  fi
else
  echo "  ✅ no direct internet route (as expected)"
fi

# 4. The runner can USE the route. Two distinct things, both required:
#    4a. GIT (not just curl) can reach the git host through the proxy. git-over-
#        https uses CONNECT, so this exercises tinyproxy's ConnectPort path that a
#        plain GET does not.
#    4b. The RUNNER CONTAINER actually carries the proxy variables — the runner
#        forwards them to its git/build children, so if they are absent from the
#        container there is no route to forward.
echo "▶ [4/4] the runner can use the route…"
if docker run --rm --network "$NET_NAME" -e https_proxy="$PROXY" -e http_proxy="$PROXY" \
     alpine/git:latest ls-remote --heads "$GIT_HOST/git/git" >/dev/null 2>&1; then
  echo "  ✅ git reaches the git host through the proxy (CONNECT works)"
else
  echo "  ❌ git could NOT reach the git host through the proxy."
  echo "     curl may still pass (plain GET) while git fails: git uses CONNECT,"
  echo "     so check tinyproxy's ConnectPort and the allowlist entry for the host."
  fail=1
fi

runner_cid="$(docker ps -qf name=code-runner-coding | head -1 || true)"
if [ -z "$runner_cid" ]; then
  echo "  ❌ could not find the code-runner-coding container to inspect its env"
  fail=1
else
  runner_env="$(docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' "$runner_cid" 2>/dev/null || true)"
  if printf '%s' "$runner_env" | grep -qiE '^(HTTPS_PROXY|https_proxy)='; then
    echo "  ✅ the runner container carries the proxy variables to forward"
  else
    echo "  ❌ the runner container has NO HTTP(S)_PROXY set — it has no route to"
    echo "     forward to git or to a build's dependency fetch, and every clone"
    echo "     will fail with a network error."
    fail=1
  fi
fi

echo
if [ "$fail" -eq 0 ]; then
  echo "✅ containment smoke PASSED — safe to enable code PRs."
else
  echo "❌ containment smoke FAILED — do NOT enable code PRs until fixed."
  exit 1
fi
