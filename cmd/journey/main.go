package main

// A standalone entry point for the journey check, for development and CI.
//
// The same code is reachable as `go-one-camp journey` inside the shipped
// container, which is how a customer runs it; this exists so the repo has a
// plain `go run ./cmd/journey` that needs no server environment.

import (
	"os"

	journey "github.com/akashc777/OneCamp/services/Journey"
)

func main() { os.Exit(journey.Main()) }
