package maintenance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/servers"
)

// Job is a scheduled maintenance job: one entry of the legacy cron/msm,
// with the four retention lines folded into prune. The host's scheduler
// runs "msm maintenance run <job>"; the job is the same command a person
// runs, under the same per-server locks.
type Job string

const (
	JobBackup  Job = "backup"  // msm all worlds backup
	JobLogroll Job = "logroll" // msm all logroll
	JobToDisk  Job = "todisk"  // msm all worlds todisk
	JobRecover Job = "recover" // msm start: restart active servers that stopped
	JobPrune   Job = "prune"   // msm maintenance prune
)

// Jobs lists every job.
var Jobs = []Job{JobBackup, JobLogroll, JobToDisk, JobRecover, JobPrune}

// ParseJob parses a job name.
func ParseJob(s string) (Job, error) {
	for _, j := range Jobs {
		if string(j) == s {
			return j, nil
		}
	}
	return "", fmt.Errorf("there is no maintenance job %q (use %s)", s, joinJobs())
}

func joinJobs() string {
	s := ""
	for i, j := range Jobs {
		if i > 0 {
			s += ", "
		}
		s += string(j)
	}
	return s
}

// DefaultTimeout is how long a job may run. Each is shorter than the
// legacy cadence that starts the next run, so a stuck run is stopped
// before the scheduler tries again, and shorter than the TimeoutStartSec
// the example units give the whole process.
func (j Job) DefaultTimeout() time.Duration {
	switch j {
	case JobBackup:
		return 4 * time.Hour
	case JobToDisk:
		return 20 * time.Minute
	case JobRecover:
		return 30 * time.Minute
	case JobPrune:
		return time.Hour
	default:
		return 15 * time.Minute
	}
}

var (
	// ErrTimeout is a job that ran past its timeout.
	ErrTimeout = errors.New("maintenance: the job exceeded its timeout")
)

// RunConfig is how a job is run.
type RunConfig struct {
	Servers *servers.Manager
	// Timeout bounds the whole job; zero means the job's default.
	Timeout time.Duration
	// Grace is how long a job that was told to stop at its timeout may
	// take to do so before it is given up on; zero means 30 seconds.
	Grace time.Duration
	// Log receives the line that records how the job was started.
	Log io.Writer
	// Skipped receives the note that the job overlapped its previous run.
	Skipped io.Writer
}

// Run runs fn as job, once at a time. If the previous run of the same job
// still holds the job's lock, this run is skipped, successfully, and says
// so: queueing behind a long run would stack up work (a RAM sync every 30
// minutes behind a two-hour backup) and the previous run is already doing
// it. Different jobs, and interactive commands, are not excluded by this
// lock; they queue on the per-server locks the commands themselves take.
//
// fn gets a context that ends at the timeout. A job still blocked after
// the grace period, for example waiting for a server's lock, is abandoned
// with ErrTimeout so the process can exit and release it.
func Run(ctx context.Context, cfg RunConfig, job Job, fn func(context.Context) error) error {
	if cfg.Timeout < 0 {
		return errors.New("maintenance: the job timeout must not be negative")
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = job.DefaultTimeout()
	}
	grace := cfg.Grace
	if grace == 0 {
		grace = 30 * time.Second
	}
	lock, err := cfg.Servers.TryLockJob(string(job))
	if errors.Is(err, filelock.ErrBusy) {
		if cfg.Skipped != nil {
			fmt.Fprintf(cfg.Skipped, "msm: maintenance %s: skipped, the previous run is still going\n", job)
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	if cfg.Log != nil {
		fmt.Fprintf(cfg.Log, "msm: maintenance %s: euid=%d timeout=%s\n", job, cfg.Servers.EUID(), timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case err := <-done:
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
			return fmt.Errorf("%w (%s): %v", ErrTimeout, timeout, err)
		}
		return err
	case <-ctx.Done():
	}
	if ctx.Err() == context.DeadlineExceeded {
		select {
		case err := <-done:
			return fmt.Errorf("%w (%s): %v", ErrTimeout, timeout, err)
		case <-time.After(grace):
			return fmt.Errorf("%w (%s) and it did not stop within %s", ErrTimeout, timeout, grace)
		}
	}
	// Interrupted by a signal: let the job finish its cleanup.
	return <-done
}
