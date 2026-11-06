package helpers

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every deployed Postgres has the shared memory its parallel reads need.
//
// WHY THIS IS A TEST. Postgres runs a big read in parallel, and the workers of a parallel hash join
// share their hash table in dynamic shared memory, which in the postgres image is /dev/shm. Docker
// gives a container a 64M /dev/shm unless its compose service sets shm_size, and neither deployed
// file did. A 100,000-row table's bar summaries join the cell index once per summed field, so three
// such reads at once (two people opening the table, or a page asking twice) ran /dev/shm out:
// "could not resize shared memory segment ... No space left on device" (SQLSTATE 53100), and about
// half the answers came back without their sums, at any CPU count (V-525,
// logs/tables-190/trace-v525a.out). Nothing in the file shows the 64M: the default is invisible
// in review, which is what a test is for.
const postgresShmFloor = 256 << 20

var (
	// A service's image is composeImageLine (imagePinGuard_test.go).
	composeShmLine = regexp.MustCompile(`^\s+shm_size:\s*"?([^\s"#]+)"?\s*(?:#.*)?$`)
	// As compose reads a size (go-units RAMInBytes): binary multiples, "i" and "b" optional.
	composeByteSize = regexp.MustCompile(`^(?i)([0-9]+(?:\.[0-9]+)?) ?([kmgtp])?i?b?$`)
)

// composeBytes is a compose byte size ("256m", "1GiB", "268435456") in bytes; false when it isn't one.
func composeBytes(s string) (int64, bool) {
	m := composeByteSize.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, false
	}
	if m[2] != "" {
		for range strings.Index("kmgtp", strings.ToLower(m[2])) + 1 {
			n *= 1024
		}
	}
	return int64(n), true
}

func TestComposeBytesReadsComposeSizes(t *testing.T) {
	for in, want := range map[string]int64{"256m": 256 << 20, "256MB": 256 << 20, "256MiB": 256 << 20, "1g": 1 << 30, "1.5g": 3 << 29,
		"268435456": 268435456, "64k": 64 << 10, "64 kb": 64 << 10} {
		if got, ok := composeBytes(in); !ok || got != want {
			t.Errorf("composeBytes(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "lots", "-1m", "256x", "m"} {
		if _, ok := composeBytes(in); ok {
			t.Errorf("composeBytes(%q) took a size compose refuses", in)
		}
	}
}

func TestDeployedPostgresHasSharedMemoryForParallelReads(t *testing.T) {
	for _, path := range deployedComposeFiles {
		t.Run(strings.TrimPrefix(path, "../"), func(t *testing.T) {
			images := serviceSettings(t, path, composeImageLine)
			shm := serviceSettings(t, path, composeShmLine)
			var postgres, offenders []string
			for svc, image := range images {
				if !strings.HasPrefix(image, "postgres:") && image != "postgres" {
					continue
				}
				postgres = append(postgres, svc)
				size, ok := composeBytes(shm[svc])
				switch {
				case shm[svc] == "":
					offenders = append(offenders, fmt.Sprintf("%s (no shm_size: Docker's 64M)", svc))
				case !ok:
					offenders = append(offenders, fmt.Sprintf("%s (shm_size %q isn't a size)", svc, shm[svc]))
				case size < postgresShmFloor:
					offenders = append(offenders, fmt.Sprintf("%s (shm_size %s, below %dM)", svc, shm[svc], postgresShmFloor>>20))
				}
			}
			if len(postgres) == 0 {
				t.Fatalf("%s runs no postgres image: the parser no longer fits the file, or the database moved", path)
			}
			sort.Strings(offenders)
			if len(offenders) > 0 {
				t.Errorf("%d Postgres service(s) would run their parallel reads out of /dev/shm (SQLSTATE 53100, "+
					"V-525). Set shm_size to at least %dM:\n  %s", len(offenders), postgresShmFloor>>20, strings.Join(offenders, "\n  "))
			}
		})
	}
}
