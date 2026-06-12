package cmd

import (
	"context"
	"time"

	"github.com/lunarhue/libs-go/log"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"

	"github.com/LunarHUE/MLS-Grid-Sync/sync"
)

// Worker subcommand flags. Both default to "unbounded" — the existing
// long-running daemon behavior is preserved.
var (
	workerMaxJobs int
	workerOnce    bool
)

var workerCmd = &cobra.Command{
	Use:   "worker",
	Short: "Start the background S3 attachment uploader",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()

		svc, db, err := setupService(ctx)
		if err != nil {
			return err
		}
		defer db.Close()

		// Media downloads are paced by their own limiter — distinct from the
		// MLS OData API limiter in mls/client.go. See Phase 4 plan §5.
		rps := appConfig.MLS.MediaDownloadRPS
		if rps <= 0 {
			rps = 1.0
		}
		limiter := rate.NewLimiter(rate.Limit(rps), 1)

		worker := sync.NewAttachmentWorker(svc,
			sync.WithMediaLimiter(limiter),
			sync.WithKeyPrefix(appConfig.Storage.KeyPrefix),
		)
		log.Infof("Starting attachment worker daemon (worker_id=%s, media_rps=%.2f)...", worker.WorkerID(), rps)
		return runWorkerLoop(ctx, worker, workerLoopOpts{
			maxJobs:      workerMaxJobs,
			once:         workerOnce,
			pollInterval: 2 * time.Second,
			errBackoff:   5 * time.Second,
		})
	},
}

// workerLoopOpts bundles the bounding flags so the loop can be unit-
// tested against a stub Run without spinning up a Service. Counts are
// in-process: terminal outcomes this worker performed, never by
// querying the table (prior test rows must not satisfy the bound).
type workerLoopOpts struct {
	maxJobs int
	once    bool
	// pollInterval is the inter-cycle sleep when idle/busy. Zero
	// means "no sleep" — only the test path sets it; prod uses 2s.
	pollInterval time.Duration
	// errBackoff is the sleep after a Run error. Zero means "no
	// sleep". Prod uses 5s.
	errBackoff time.Duration
}

// workerRunner is the subset of *sync.AttachmentWorker the loop needs.
// Lets cmd/worker_test.go drive runWorkerLoop with a stub.
type workerRunner interface {
	Run(ctx context.Context) (sync.RunResult, error)
	WorkerID() string
}

func runWorkerLoop(ctx context.Context, worker workerRunner, opts workerLoopOpts) error {
	idle := sync.NewIdleTracker()

	var totalSucceeded, totalRetrying, totalPermFailed, totalLostCAS int64
	terminalReached := func() bool {
		if opts.maxJobs <= 0 {
			return false
		}
		return totalSucceeded+totalPermFailed >= int64(opts.maxJobs)
	}
	summarize := func(reason string) {
		log.Infof("worker: %s (%d succeeded, %d permanently-failed, %d retrying transitions, %d lost-cas) — exiting",
			reason, totalSucceeded, totalPermFailed, totalRetrying, totalLostCAS)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			result, err := worker.Run(ctx)
			if err != nil {
				log.Errorf("Worker batch error: %v", err)
				idle.Record(true)
				if opts.once {
					summarize("--once cycle errored")
					return err
				}
				if opts.errBackoff > 0 {
					time.Sleep(opts.errBackoff)
				}
				continue
			}
			totalSucceeded += result.Succeeded
			totalRetrying += result.Retrying
			totalPermFailed += result.PermanentlyFailed
			totalLostCAS += result.LostCAS

			if idle.Record(result.Worked) {
				log.Infof("worker %s: no pending jobs, idling", worker.WorkerID())
			}
			if opts.once {
				summarize("--once cycle complete")
				return nil
			}
			if terminalReached() {
				summarize("--max-jobs " + itoa(opts.maxJobs) + " reached")
				return nil
			}
			if opts.pollInterval > 0 {
				time.Sleep(opts.pollInterval)
			}
		}
	}
}

// itoa avoids dragging strconv into the import block for a single call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func init() {
	workerCmd.Flags().IntVar(&workerMaxJobs, "max-jobs", 0,
		"Exit after N terminal outcomes (succeeded + permanently-failed). 0 = unlimited.")
	workerCmd.Flags().BoolVar(&workerOnce, "once", false,
		"Run exactly one claim cycle then exit.")
	rootCmd.AddCommand(workerCmd)
}
