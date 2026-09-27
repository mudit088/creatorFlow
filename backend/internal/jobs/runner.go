// Package jobs runs the periodic work the schema has been promising: expiring
// abandoned checkouts, sweeping uploads that were never completed, and deleting
// raw analytics past its retention window.
//
// It runs inside the API process rather than as a separate deployment. That is a
// deliberate choice for this size of system — a second binary, its own image,
// its own alerting and its own deploy is real operational weight, and these jobs
// are three SQL statements on a timer. The tradeoff is that they compete with
// request traffic for the same database pool, which is why every one of them
// deletes in bounded batches. When the work grows past that, the jobs move out;
// the Job interface below is what makes that a wiring change rather than a
// rewrite.
package jobs

import (
	"context"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// Job is one unit of periodic work.
type Job struct {
	Name     string
	Interval time.Duration
	// Timeout bounds a single run. It also sets how long the lock is held, so a
	// run that dies without releasing cannot block the job forever.
	Timeout time.Duration
	// Run reports how many rows it touched, which is the only interesting thing
	// about a cleanup job in a log line.
	Run func(ctx context.Context) (int64, error)
}

// releaseIfOwner deletes the lock only when it still holds our token.
//
// The check matters: if a run overruns its lock TTL, another instance may have
// already taken the lock, and a naive DEL would release someone else's — letting
// a third instance start while the second is still working. Compare-and-delete
// has to be atomic, so it is a script.
var releaseIfOwner = redis.NewScript(`
	if redis.call('GET', KEYS[1]) == ARGV[1] then
		return redis.call('DEL', KEYS[1])
	end
	return 0
`)

type Runner struct {
	rdb  *redis.Client
	jobs []Job

	wg     sync.WaitGroup
	cancel context.CancelFunc
}

func New(rdb *redis.Client) *Runner {
	return &Runner{rdb: rdb}
}

func (r *Runner) Add(j Job) { r.jobs = append(r.jobs, j) }

// Start launches one goroutine per job and returns immediately.
func (r *Runner) Start(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel

	for _, job := range r.jobs {
		r.wg.Add(1)
		go func(j Job) {
			defer r.wg.Done()
			r.loop(ctx, j)
		}(job)
	}
}

// Stop signals the loops and waits for any in-flight run to finish.
func (r *Runner) Stop() {
	if r.cancel == nil {
		return
	}
	r.cancel()
	r.wg.Wait()
}

func (r *Runner) loop(ctx context.Context, j Job) {
	// A random first delay. Without it, every instance in a deployment wakes on
	// the same schedule and contends for the same lock at the same instant —
	// harmless here, but the same pattern at scale is a thundering herd, and
	// spreading the start costs one line.
	initial := time.Duration(rand.Int63n(int64(j.Interval/2 + 1)))

	timer := time.NewTimer(initial)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			r.runOnce(ctx, j)
			timer.Reset(j.Interval)
		}
	}
}

// runOnce takes the lock, runs the job, and releases.
//
// Failing to acquire is the normal case, not an error: with three API instances
// two of them will lose every time, and that is exactly what the lock is for.
func (r *Runner) runOnce(ctx context.Context, j Job) {
	token, ok := r.acquire(ctx, j)
	if !ok {
		return
	}
	defer r.release(ctx, j.Name, token)

	runCtx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()

	started := time.Now()
	affected, err := j.Run(runCtx)
	if err != nil {
		slog.Error("job failed", "job", j.Name, "error", err, "duration", time.Since(started))
		return
	}

	// Only log when something happened. A cleanup job that says "deleted 0 rows"
	// every ten minutes trains everyone to ignore its output, which is precisely
	// when a real number needs to be noticed.
	if affected > 0 {
		slog.Info("job completed", "job", j.Name, "rows", affected, "duration", time.Since(started))
	} else {
		slog.Debug("job completed", "job", j.Name, "rows", 0, "duration", time.Since(started))
	}
}

// acquire takes a lock that expires on its own.
//
// With no Redis there is no lock, and the job runs anyway. That is the right
// default for a single-instance deployment — refusing to clean up because a
// cache is down would be worse than the duplicate work a lock prevents. Every
// job below is idempotent, so the worst case of a double run is wasted effort.
func (r *Runner) acquire(ctx context.Context, j Job) (string, bool) {
	if r.rdb == nil {
		return "", true
	}

	token := uuid.NewString()
	// The lock outlives the job's own timeout by a margin, so a run that is
	// still finishing cannot have its lock taken by the next instance.
	ttl := j.Timeout + 30*time.Second

	ok, err := r.rdb.SetNX(ctx, lockKey(j.Name), token, ttl).Result()
	if err != nil {
		slog.Warn("job lock unavailable, running anyway", "job", j.Name, "error", err)
		return "", true
	}
	if !ok {
		slog.Debug("job already running elsewhere", "job", j.Name)
		return "", false
	}
	return token, true
}

func (r *Runner) release(ctx context.Context, name, token string) {
	if r.rdb == nil || token == "" {
		return
	}

	// A fresh context: the parent may already be cancelled by shutdown, and a
	// lock that is not released then sits until its TTL expires, delaying the
	// job on the next instance for no reason.
	releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := releaseIfOwner.Run(releaseCtx, r.rdb, []string{lockKey(name)}, token).Err(); err != nil {
		slog.Warn("failed to release job lock; it will expire on its own", "job", name, "error", err)
	}
}

func lockKey(name string) string { return "jobs:lock:" + name }
