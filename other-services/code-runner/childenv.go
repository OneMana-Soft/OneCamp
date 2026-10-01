package main

import (
	"os"
	"strings"
)

// childenv.go — the environment handed to every child process the coding runner
// spawns: git, and the repo's own build/test toolchain.
//
// Those envs are built EXPLICITLY rather than inherited, so a secret in the
// runner's own environment (its shared runner token, the LLM proxy token) can
// never reach a build script or a test file the model just wrote. That is the
// right default, and it had one casualty worth naming: the shipped topology
// gives the coding runner NO direct internet — its egress is forced through an
// allowlisting proxy advertised only in HTTP(S)_PROXY — so building the child
// env from scratch also removed the single route out. Every clone, every push,
// and every dependency fetch then failed with a network error in exactly the
// hardened configuration the feature is meant to run in, while the containment
// smoke check kept passing because it exercises the proxy from its own throwaway
// container rather than through the runner.
//
// proxyEnv restores that route and nothing else. Proxy coordinates are
// deployment topology, not secrets.

// proxyEnvNames are the variables that carry the egress route. BOTH cases are
// forwarded because the toolchains disagree about which they read: Go and curl
// prefer the lower-case forms, git/libcurl accept either, and some npm/pip
// versions honour only one. Forwarding both means an operator who sets either
// gets a working route instead of a silent no-egress failure.
var proxyEnvNames = []string{
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

// passthroughEnv returns KEY=VALUE for each named variable that is actually set
// (and non-blank) in this process, skipping the rest. Generic on purpose: the
// caller names exactly what it wants to forward, so nothing is inherited by
// accident and the allowlist stays readable at the call site.
func passthroughEnv(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok && strings.TrimSpace(v) != "" {
			out = append(out, n+"="+v)
		}
	}
	return out
}

// proxyEnv is the egress route for a child process: the proxy variables set on
// the runner, and nothing else. Empty when the runner has no proxy configured
// (a direct-internet dev setup), so behaviour there is unchanged.
func proxyEnv() []string { return passthroughEnv(proxyEnvNames...) }
