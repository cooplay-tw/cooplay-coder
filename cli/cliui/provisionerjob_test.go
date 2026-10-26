package cliui_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/v2/cli/cliui"
	"github.com/coder/coder/v2/coderd/database/dbtime"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/testutil"
	"github.com/coder/coder/v2/testutil/expecter"
	"github.com/coder/serpent"
)

// This cannot be ran in parallel because it uses a signal.
// nolint:tparallel
func TestProvisionerJob(t *testing.T) {
	t.Run("NoLogs", func(t *testing.T) {
		t.Parallel()

		test := newProvisionerJob(t)

		ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
		defer cancel()

		testutil.Go(t, func() {
			<-test.Next
			test.JobMutex.Lock()
			test.Job.Status = codersdk.ProvisionerJobRunning
			now := dbtime.Now()
			test.Job.StartedAt = &now
			test.JobMutex.Unlock()
			<-test.Next
			test.JobMutex.Lock()
			test.Job.Status = codersdk.ProvisionerJobSucceeded
			now = dbtime.Now()
			test.Job.CompletedAt = &now
			close(test.Logs)
			test.JobMutex.Unlock()
		})
		testutil.Eventually(ctx, t, func(ctx context.Context) (done bool) {
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
			test.Next <- struct{}{}
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateRunning)
			test.Next <- struct{}{}
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateRunning)
			return true
		}, testutil.IntervalFast)
	})

	t.Run("Stages", func(t *testing.T) {
		t.Parallel()

		test := newProvisionerJob(t)
		ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
		defer cancel()

		testutil.Go(t, func() {
			<-test.Next
			test.JobMutex.Lock()
			test.Job.Status = codersdk.ProvisionerJobRunning
			now := dbtime.Now()
			test.Job.StartedAt = &now
			test.Logs <- codersdk.ProvisionerJobLog{
				CreatedAt: dbtime.Now(),
				Stage:     "Something",
			}
			test.JobMutex.Unlock()
			<-test.Next
			test.JobMutex.Lock()
			test.Job.Status = codersdk.ProvisionerJobSucceeded
			now = dbtime.Now()
			test.Job.CompletedAt = &now
			close(test.Logs)
			test.JobMutex.Unlock()
		})
		testutil.Eventually(ctx, t, func(ctx context.Context) (done bool) {
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
			test.Next <- struct{}{}
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
			test.Stdout.ExpectMatch(ctx, "Something")
			test.Next <- struct{}{}
			test.Stdout.ExpectMatch(ctx, "Something")
			return true
		}, testutil.IntervalFast)
	})

	t.Run("Queue Position", func(t *testing.T) {
		t.Parallel()

		stage := cliui.ProvisioningStateQueued

		tests := []struct {
			name     string
			queuePos int
			expected string
		}{
			{
				name:     "first",
				queuePos: 0,
				expected: fmt.Sprintf("%s$", stage),
			},
			{
				name:     "next",
				queuePos: 1,
				expected: fmt.Sprintf(`%s %s$`, stage, regexp.QuoteMeta("(next)")),
			},
			{
				name:     "other",
				queuePos: 4,
				expected: fmt.Sprintf(`%s %s$`, stage, regexp.QuoteMeta("(position: 4)")),
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				test := newProvisionerJob(t)
				test.JobMutex.Lock()
				test.Job.QueuePosition = tc.queuePos
				test.Job.QueueSize = tc.queuePos
				test.JobMutex.Unlock()

				ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
				defer cancel()

				testutil.Go(t, func() {
					<-test.Next
					test.JobMutex.Lock()
					test.Job.Status = codersdk.ProvisionerJobRunning
					now := dbtime.Now()
					test.Job.StartedAt = &now
					test.JobMutex.Unlock()
					<-test.Next
					test.JobMutex.Lock()
					test.Job.Status = codersdk.ProvisionerJobSucceeded
					now = dbtime.Now()
					test.Job.CompletedAt = &now
					close(test.Logs)
					test.JobMutex.Unlock()
				})
				testutil.Eventually(ctx, t, func(ctx context.Context) (done bool) {
					test.Stdout.ExpectRegexMatch(ctx, tc.expected)
					test.Next <- struct{}{}
					test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued) // step completed
					test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateRunning)
					test.Next <- struct{}{}
					test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateRunning)
					return true
				}, testutil.IntervalFast)
			})
		}
	})

	t.Run("CapacityWait", func(t *testing.T) {
		t.Parallel()

		position := func(p int) *int { return &p }
		test := newWorkspaceBuildJob(t, &codersdk.WorkspaceBuildAdmission{
			Status:        codersdk.WorkspaceBuildAdmissionStatusWaiting,
			Reason:        codersdk.WorkspaceBuildAdmissionReasonWaitingForCapacity,
			QueuePosition: position(2),
			QueuedAt:      dbtime.Now(),
		})
		ctx := testutil.Context(t, testutil.WaitShort)

		testutil.Go(t, func() {
			<-test.Next
			test.JobMutex.Lock()
			test.Build.Admission.Reason = "2 workspaces ahead of you"
			test.Build.Admission.QueuePosition = position(1)
			test.JobMutex.Unlock()
			<-test.Next
			test.JobMutex.Lock()
			now := dbtime.Now()
			test.Build.Admission.Status = codersdk.WorkspaceBuildAdmissionStatusGranted
			test.Build.Admission.Reason = ""
			test.Build.Admission.QueuePosition = nil
			test.Build.Admission.GrantedAt = &now
			// Once granted, the build competes for a provisioner.
			test.Job.QueuePosition = 1
			test.Job.QueueSize = 1
			test.JobMutex.Unlock()
			<-test.Next
			test.JobMutex.Lock()
			now = dbtime.Now()
			test.Build.Admission.Status = codersdk.WorkspaceBuildAdmissionStatusAcquired
			test.Job.Status = codersdk.ProvisionerJobRunning
			test.Job.StartedAt = &now
			test.JobMutex.Unlock()
			<-test.Next
			test.JobMutex.Lock()
			now = dbtime.Now()
			test.Job.Status = codersdk.ProvisionerJobSucceeded
			test.Job.CompletedAt = &now
			close(test.Logs)
			test.JobMutex.Unlock()
		})

		test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
		// The default reason only restates the message.
		test.Stdout.ExpectRegexMatch(ctx, `Waiting for capacity \(queue position 2\)\r?\n`)
		test.Next <- struct{}{}
		test.Stdout.ExpectMatch(ctx, "Waiting for capacity: 2 workspaces ahead of you (queue position 1)")
		test.Next <- struct{}{}
		test.Stdout.ExpectMatch(ctx, "Capacity granted; waiting for a provisioner")
		test.Stdout.ExpectMatch(ctx, "Queued (next)")
		test.Next <- struct{}{}
		test.Stdout.ExpectNoMatchBefore(ctx, "capacity", cliui.ProvisioningStateRunning)
		test.Next <- struct{}{}
		require.NoError(t, testutil.RequireReceive(ctx, t, test.Result))
	})

	t.Run("CapacityRejected", func(t *testing.T) {
		t.Parallel()

		test := newWorkspaceBuildJob(t, &codersdk.WorkspaceBuildAdmission{
			Status:   codersdk.WorkspaceBuildAdmissionStatusWaiting,
			Reason:   codersdk.WorkspaceBuildAdmissionReasonWaitingForCapacity,
			QueuedAt: dbtime.Now(),
		})
		ctx := testutil.Context(t, testutil.WaitShort)

		testutil.Go(t, func() {
			<-test.Next
			test.JobMutex.Lock()
			now := dbtime.Now()
			test.Build.Admission.Status = codersdk.WorkspaceBuildAdmissionStatusRejected
			test.Build.Admission.Reason = "quota: profile large is not allowed"
			test.Job.Status = codersdk.ProvisionerJobFailed
			test.Job.StartedAt = &now
			test.Job.CompletedAt = &now
			test.Job.Error = "admission rejected: quota: profile large is not allowed"
			close(test.Logs)
			test.JobMutex.Unlock()
		})

		test.Stdout.ExpectMatch(ctx, "Waiting for capacity")
		test.Next <- struct{}{}
		test.Stdout.ExpectMatch(ctx, "Capacity request rejected: quota: profile large is not allowed")
		err := testutil.RequireReceive(ctx, t, test.Result)
		var jobErr *cliui.ProvisionerJobError
		require.ErrorAs(t, err, &jobErr)
		require.Equal(t, "admission rejected: quota: profile large is not allowed", jobErr.Message)
	})

	t.Run("BuildWithoutAdmission", func(t *testing.T) {
		t.Parallel()

		test := newWorkspaceBuildJob(t, nil)
		ctx := testutil.Context(t, testutil.WaitShort)

		testutil.Go(t, func() {
			<-test.Next
			test.JobMutex.Lock()
			now := dbtime.Now()
			test.Job.Status = codersdk.ProvisionerJobRunning
			test.Job.StartedAt = &now
			test.JobMutex.Unlock()
			<-test.Next
			test.JobMutex.Lock()
			now = dbtime.Now()
			test.Job.Status = codersdk.ProvisionerJobSucceeded
			test.Job.CompletedAt = &now
			close(test.Logs)
			test.JobMutex.Unlock()
		})

		test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
		test.Next <- struct{}{}
		test.Stdout.ExpectNoMatchBefore(ctx, "Waiting for capacity", cliui.ProvisioningStateRunning)
		test.Next <- struct{}{}
		require.NoError(t, testutil.RequireReceive(ctx, t, test.Result))
	})

	// This cannot be ran in parallel because it uses a signal.
	// nolint:paralleltest
	t.Run("Cancel", func(t *testing.T) {
		t.Skip("This test issues an interrupt signal which will propagate to the test runner.")

		if runtime.GOOS == "windows" {
			// Sending interrupt signal isn't supported on Windows!
			t.SkipNow()
		}

		test := newProvisionerJob(t)

		ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
		defer cancel()

		testutil.Go(t, func() {
			<-test.Next
			currentProcess, err := os.FindProcess(os.Getpid())
			assert.NoError(t, err)
			err = currentProcess.Signal(os.Interrupt)
			assert.NoError(t, err)
			<-test.Next
			test.JobMutex.Lock()
			test.Job.Status = codersdk.ProvisionerJobCanceled
			now := dbtime.Now()
			test.Job.CompletedAt = &now
			close(test.Logs)
			test.JobMutex.Unlock()
		})
		testutil.Eventually(ctx, t, func(ctx context.Context) (done bool) {
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
			test.Next <- struct{}{}
			test.Stdout.ExpectMatch(ctx, "Gracefully canceling")
			test.Next <- struct{}{}
			test.Stdout.ExpectMatch(ctx, cliui.ProvisioningStateQueued)
			return true
		}, testutil.IntervalFast)
	})
}

type provisionerJobTest struct {
	Next     chan struct{}
	Job      *codersdk.ProvisionerJob
	JobMutex *sync.Mutex
	Logs     chan codersdk.ProvisionerJobLog
	Stdout   *expecter.Expecter
	// Build is the workspace build that owns Job. Only workspace build
	// watches see its admission.
	Build *codersdk.WorkspaceBuild
	// Result receives the error of a workspace build watch.
	Result chan error
}

func newProvisionerJob(t *testing.T) provisionerJobTest {
	test := startProvisionerJob(t, nil, func(opts *cliui.ProvisionerJobOptions, snapshot func() codersdk.WorkspaceBuild) {
		opts.Fetch = func() (codersdk.ProvisionerJob, error) {
			return snapshot().Job, nil
		}
	})
	t.Cleanup(func() {
		err := <-test.Result
		if err != nil {
			assert.ErrorIs(t, err, cliui.ErrCanceled)
		}
	})
	return test
}

// newWorkspaceBuildJob watches a workspace build with the given admission.
// The test must receive the watch result from Result.
func newWorkspaceBuildJob(t *testing.T, admission *codersdk.WorkspaceBuildAdmission) provisionerJobTest {
	return startProvisionerJob(t, admission, func(opts *cliui.ProvisionerJobOptions, snapshot func() codersdk.WorkspaceBuild) {
		opts.FetchBuild = func() (codersdk.WorkspaceBuild, error) {
			return snapshot(), nil
		}
	})
}

// startProvisionerJob runs a job watch whose fetch function setFetch installs
// from a snapshot of the build under test.
func startProvisionerJob(
	t *testing.T,
	admission *codersdk.WorkspaceBuildAdmission,
	setFetch func(opts *cliui.ProvisionerJobOptions, snapshot func() codersdk.WorkspaceBuild),
) provisionerJobTest {
	build := &codersdk.WorkspaceBuild{
		Job: codersdk.ProvisionerJob{
			Status:    codersdk.ProvisionerJobPending,
			CreatedAt: dbtime.Now(),
		},
		Admission: admission,
	}
	jobLock := sync.Mutex{}
	logs := make(chan codersdk.ProvisionerJobLog, 1)
	opts := cliui.ProvisionerJobOptions{
		FetchInterval: time.Millisecond,
		Cancel: func() error {
			return nil
		},
		Logs: func() (<-chan codersdk.ProvisionerJobLog, io.Closer, error) {
			return logs, closeFunc(func() error {
				return nil
			}), nil
		},
	}
	setFetch(&opts, func() codersdk.WorkspaceBuild {
		jobLock.Lock()
		defer jobLock.Unlock()
		snapshot := *build
		if build.Admission != nil {
			copied := *build.Admission
			snapshot.Admission = &copied
		}
		return snapshot
	})
	cmd := &serpent.Command{
		Handler: func(inv *serpent.Invocation) error {
			return cliui.ProvisionerJob(inv.Context(), inv.Stdout, opts)
		},
	}
	inv := cmd.Invoke()

	stdout := expecter.NewAttachedToInvocation(t, inv)
	result := make(chan error, 1)
	go func() {
		result <- inv.WithContext(context.Background()).Run()
	}()
	return provisionerJobTest{
		Next:     make(chan struct{}),
		Job:      &build.Job,
		JobMutex: &jobLock,
		Logs:     logs,
		Stdout:   stdout,
		Build:    build,
		Result:   result,
	}
}

type closeFunc func() error

func (c closeFunc) Close() error {
	return c()
}
