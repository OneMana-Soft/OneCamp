package pgexec

import "github.com/lib/pq"

// Array is the slice a ([]string, []int64, ...) as one Postgres array
// argument, for text such as `id = ANY($1::uuid[])`; a nil slice is NULL.
// Text written in domain/ hands its lists to Query and Exec through this, so
// the layers above models/ never import a store driver.
func Array(a any) any { return pq.Array(a) }
