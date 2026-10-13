package business

// Each run of a job does its work once.
//
// A job that did its work and then wasn't marked done (a database hiccup, a
// worker that died between the two) stays "running", and once its lock is
// old another worker takes it: a reminder or a scheduled message went out
// again. Each run of a job (the job at its time; a repeating job's next run is
// another) is marked in Redis as soon as its handler succeeds, before the job
// is marked done, and a run that finds its mark only does the marking.
//
// Best effort by design: with Redis unreachable the job runs, as it always
// did. The mark is the run's idempotency key; it outlives every retry.

import (
	"context"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	jobModel "github.com/akashc777/OneCamp/models/postgres/ScheduledJob"
)

const (
	ranKeyPrefix = "scheduler:ran:"
	ranKeep      = 14 * 24 * time.Hour
)

// ranKey names one run of a job: the job and the time it was due.
func ranKey(job *jobModel.ScheduledJob) string {
	return ranKeyPrefix + job.Id.String() + ":" + strconv.FormatInt(job.RunAt.UTC().UnixNano(), 10)
}

// alreadyRan says whether this run of job did its work before.
func alreadyRan(ctx context.Context, job *jobModel.ScheduledJob) bool {
	c := redisInit.RedisClient
	if c == nil {
		return false
	}
	n, err := c.Exists(ctx, ranKey(job)).Result()
	if err != nil {
		helpers.LogErrorWithContext(ctx, "Scheduler could not read whether job %s already ran, running it err: %+v", job.Id, err)
		return false
	}
	return n > 0
}

// markRan records that this run of job did its work.
func markRan(ctx context.Context, job *jobModel.ScheduledJob) {
	c := redisInit.RedisClient
	if c == nil {
		return
	}
	if err := c.Set(ctx, ranKey(job), 1, ranKeep).Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "Scheduler could not mark job %s as run err: %+v", job.Id, err)
	}
}
