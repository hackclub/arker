package canary

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
)

// QueueName is the dedicated River queue canary sweeps run on.
//
// Its own queue, with one worker, for two reasons: a sweep must never take
// capture slots away from real user archives, and two overlapping sweeps must
// never double the unattended traffic a platform sees from Arker.
const QueueName = "canary"

// JobArgs is the River payload for a canary sweep.
type JobArgs struct {
	// Trigger is "schedule" for the periodic job, "manual" for an operator.
	Trigger string `json:"trigger"`
	// Platform optionally narrows the sweep.
	Platform string `json:"platform,omitempty"`
}

// Kind returns the River job kind.
func (JobArgs) Kind() string { return "canary_sweep" }

// Worker runs canary sweeps off the queue.
type Worker struct {
	river.WorkerDefaults[JobArgs]
	runner *Runner
}

// NewWorker wraps a Runner as a River worker.
func NewWorker(runner *Runner) *Worker { return &Worker{runner: runner} }

// Timeout bounds a whole sweep: every probe's ceiling, plus slack for the
// database work between them.
func (w *Worker) Timeout(*river.Job[JobArgs]) time.Duration {
	probes, _ := w.runner.Probes()
	count := len(probes)
	if count == 0 {
		count = 1
	}
	return time.Duration(count)*w.runner.Config().EffectiveProbeTimeout() + 5*time.Minute
}

// Work runs one sweep.
//
// A failing probe is data, not a job error: returning an error would make
// River retry the sweep, which would re-archive every probe URL and turn one
// broken platform into repeated unattended traffic against all of them. The
// failure is already recorded in canary_runs and logged at ERROR. Only a
// refusal to run at all (the paid-archiver guard tripping) is an error.
func (w *Worker) Work(ctx context.Context, job *river.Job[JobArgs]) error {
	trigger := job.Args.Trigger
	if trigger == "" {
		trigger = TriggerSchedule
	}

	// Scheduled sweeps are gated on the schedule being configured, so an
	// enqueued job left over from a previous configuration (or inserted by
	// hand in the River UI) cannot restart recurring canaries after an
	// operator has turned them off.
	if trigger == TriggerSchedule && !w.runner.Config().ScheduleEnabled() {
		slog.Warn("Ignoring scheduled canary sweep: canaries are disabled", "env", ScheduleEnvVar)
		return nil
	}

	_, err := w.runner.RunSweep(ctx, SweepOptions{Trigger: trigger, Platform: job.Args.Platform})
	if errors.Is(err, ErrSweepInProgress) {
		slog.Warn("Skipping canary sweep: another sweep is still running", "trigger", trigger)
		return nil
	}
	return err
}

// PeriodicJob builds the River periodic job for recurring canaries, or nil
// when CANARY_SCHEDULE is unset.
//
// Returning nil for a disabled schedule is deliberate: the caller registers
// nothing at all rather than registering a job that decides not to work. There
// is no periodic entry in River, nothing in the River UI, and no path by which
// a sweep can start on its own.
//
// RunOnStart is false so a deploy or a crash-loop cannot turn a restart into a
// burst of probe traffic.
func PeriodicJob(cfg Config) *river.PeriodicJob {
	if !cfg.ScheduleEnabled() {
		return nil
	}
	return river.NewPeriodicJob(
		river.PeriodicInterval(cfg.Interval),
		func() (river.JobArgs, *river.InsertOpts) {
			return JobArgs{Trigger: TriggerSchedule}, &river.InsertOpts{
				Queue:       QueueName,
				MaxAttempts: 1,
				Tags:        []string{"canary"},
			}
		},
		&river.PeriodicJobOpts{RunOnStart: false},
	)
}
