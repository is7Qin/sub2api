//go:build unit

package workerruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPeriodicJobRecordsSuccessAndSchedulesNextRun(t *testing.T) {
	ran := make(chan struct{}, 1)
	job, err := NewPeriodicJob(PeriodicJobSpec{
		Descriptor:     periodicDescriptor("account-expiry"),
		Interval:       20 * time.Millisecond,
		Timeout:        time.Second,
		RunImmediately: true,
		Run: func(context.Context) error {
			ran <- struct{}{}
			return nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, job.Start(context.Background()))
	defer stopPeriodicJob(t, job)

	require.Eventually(t, func() bool { return len(ran) == 1 }, time.Second, time.Millisecond)
	status := periodicStatus(t, job)
	require.Equal(t, OutcomeSuccess, status.LastOutcome)
	require.Equal(t, uint64(1), status.SuccessCount)
	require.False(t, status.NextRunAt.IsZero())
	require.False(t, status.LastRunAt.IsZero())
	require.Greater(t, status.LastDuration, time.Duration(0))
}

func TestPeriodicJobConvertsPanicToOutcome(t *testing.T) {
	job := mustNewPeriodicJob(t, func(context.Context) error { panic("boom") })
	require.NoError(t, job.Start(context.Background()))
	defer stopPeriodicJob(t, job)

	require.Eventually(t, func() bool { return periodicStatus(t, job).PanicCount == 1 }, time.Second, time.Millisecond)
	status := periodicStatus(t, job)
	require.Equal(t, OutcomePanic, status.LastOutcome)
	require.NotEmpty(t, status.LastError)
}

func TestPeriodicJobTimedOutCallbackDoesNotOverlap(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var runs atomic.Int32
	job := mustNewPeriodicJobWithTimeout(t, 10*time.Millisecond, func(context.Context) error {
		runs.Add(1)
		started <- struct{}{}
		<-release
		return nil
	})
	require.NoError(t, job.Start(context.Background()))
	<-started
	require.Eventually(t, func() bool { return periodicStatus(t, job).TimeoutCount == 1 }, time.Second, time.Millisecond)
	time.Sleep(40 * time.Millisecond)
	require.Equal(t, int32(1), runs.Load())
	close(release)
	stopPeriodicJob(t, job)
}

func TestNewPeriodicJobValidatesSpec(t *testing.T) {
	valid := PeriodicJobSpec{
		Descriptor: periodicDescriptor("account-expiry"),
		Interval:   time.Second,
		Timeout:    time.Second,
		Run:        func(context.Context) error { return nil },
	}
	for _, test := range []struct {
		name string
		edit func(*PeriodicJobSpec)
	}{
		{"interval", func(spec *PeriodicJobSpec) { spec.Interval = 0 }},
		{"timeout", func(spec *PeriodicJobSpec) { spec.Timeout = 0 }},
		{"callback", func(spec *PeriodicJobSpec) { spec.Run = nil }},
		{"descriptor", func(spec *PeriodicJobSpec) { spec.Descriptor.Name = " " }},
		{"kind", func(spec *PeriodicJobSpec) { spec.Descriptor.Kind = KindPool }},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := valid
			test.edit(&spec)
			_, err := NewPeriodicJob(spec)
			require.Error(t, err)
		})
	}
}

func TestPeriodicJobRecordsCallbackError(t *testing.T) {
	callbackErr := errors.New("failed to clean accounts")
	job := mustNewPeriodicJob(t, func(context.Context) error { return callbackErr })
	require.NoError(t, job.Start(context.Background()))
	defer stopPeriodicJob(t, job)

	require.Eventually(t, func() bool { return periodicStatus(t, job).ErrorCount == 1 }, time.Second, time.Millisecond)
	status := periodicStatus(t, job)
	require.Equal(t, OutcomeError, status.LastOutcome)
	require.Contains(t, status.LastError, callbackErr.Error())
}

func TestPeriodicJobDoesNotRunImmediatelyWhenDisabled(t *testing.T) {
	var runs atomic.Int32
	job, err := NewPeriodicJob(PeriodicJobSpec{
		Descriptor: periodicDescriptor("account-expiry"),
		Interval:   100 * time.Millisecond,
		Timeout:    time.Second,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, job.Start(context.Background()))
	defer stopPeriodicJob(t, job)

	time.Sleep(25 * time.Millisecond)
	require.Zero(t, runs.Load())
}

func TestPeriodicJobStopWhileWaitingPreventsInvocation(t *testing.T) {
	var runs atomic.Int32
	job, err := NewPeriodicJob(PeriodicJobSpec{
		Descriptor: periodicDescriptor("account-expiry"),
		Interval:   time.Second,
		Timeout:    time.Second,
		Run: func(context.Context) error {
			runs.Add(1)
			return nil
		},
	})
	require.NoError(t, err)
	require.NoError(t, job.Start(context.Background()))
	require.NoError(t, job.Stop(context.Background()))
	time.Sleep(25 * time.Millisecond)
	require.Zero(t, runs.Load())
	require.Equal(t, LifecycleStopped, job.Snapshot().Lifecycle.State)
}

func TestPeriodicJobStopCancelsCallback(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	job := mustNewPeriodicJob(t, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	})
	require.NoError(t, job.Start(context.Background()))
	<-started
	require.NoError(t, job.Stop(context.Background()))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel callback")
	}
	status := periodicStatus(t, job)
	require.Equal(t, OutcomeError, status.LastOutcome)
	require.Zero(t, status.TimeoutCount)
	require.Equal(t, LifecycleStopped, job.Snapshot().Lifecycle.State)
}

func TestPeriodicJobStopDeadlineRetainsStoppingStateForIgnoringCallback(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	job := mustNewPeriodicJob(t, func(context.Context) error {
		close(started)
		<-release
		return nil
	})
	require.NoError(t, job.Start(context.Background()))
	<-started

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := job.Stop(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	snapshot := job.Snapshot()
	require.Equal(t, StateStopping, snapshot.Lifecycle.State)
	require.True(t, periodicStatus(t, job).StillRunning)

	close(release)
	require.Eventually(t, func() bool { return job.Snapshot().Lifecycle.State == LifecycleStopped }, time.Second, time.Millisecond)
}

func periodicDescriptor(name string) Descriptor {
	return Descriptor{Name: name, Kind: KindPeriodic, CoordinationMode: CoordinationPerInstance}
}

func mustNewPeriodicJob(t *testing.T, run func(context.Context) error) *PeriodicJob {
	t.Helper()
	return mustNewPeriodicJobWithTimeout(t, time.Second, run)
}

func mustNewPeriodicJobWithTimeout(t *testing.T, timeout time.Duration, run func(context.Context) error) *PeriodicJob {
	t.Helper()
	job, err := NewPeriodicJob(PeriodicJobSpec{
		Descriptor:     periodicDescriptor("account-expiry"),
		Interval:       10 * time.Millisecond,
		Timeout:        timeout,
		RunImmediately: true,
		Run:            run,
	})
	require.NoError(t, err)
	return job
}

func periodicStatus(t *testing.T, job *PeriodicJob) PeriodicStatus {
	t.Helper()
	status, ok := job.Snapshot().Status.(PeriodicStatus)
	require.True(t, ok)
	return status
}

func stopPeriodicJob(t *testing.T, job *PeriodicJob) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, job.Stop(ctx))
}
