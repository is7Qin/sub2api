//go:build unit

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestOpsHandlerWorkerRuntimeStatusUnavailable(t *testing.T) {
	h := NewOpsHandler(newMonitoringEnabledOpsService())
	r := newOpsSystemLogTestRouter(h, true)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/status", nil))

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestOpsHandlerWorkerRuntimeStatusUnavailableWithoutOpsService(t *testing.T) {
	h := NewOpsHandler(nil)
	h.SetWorkerRuntime(newStartedRuntimeForHandler(t))
	r := newOpsSystemLogTestRouter(h, true)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/status", nil))

	require.Equal(t, http.StatusServiceUnavailable, response.Code)
}

func TestOpsHandlerWorkerRuntimeStatusMonitoringDisabled(t *testing.T) {
	h := NewOpsHandler(service.NewOpsService(nil, nil, &config.Config{
		Ops: config.OpsConfig{Enabled: false},
	}, nil, nil, nil, nil, nil, nil, nil, nil))
	h.SetWorkerRuntime(newStartedRuntimeForHandler(t))
	r := newOpsSystemLogTestRouter(h, true)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/status", nil))

	require.Equal(t, http.StatusNotFound, response.Code)
}

func TestOpsHandlerWorkerRuntimeStatusOmitsRawRuntimeErrors(t *testing.T) {
	runtime := workerruntime.NewRuntime(workerruntime.NewRegistry())
	require.NoError(t, runtime.Register(workerRuntimeSnapshotFixture{snapshot: workerruntime.Snapshot{
		Descriptor: workerruntime.Descriptor{
			Name:             "error-bearing-worker",
			Kind:             workerruntime.KindPeriodic,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Lifecycle: workerruntime.LifecycleSnapshot{
			State:     workerruntime.LifecycleFailed,
			UpdatedAt: time.Date(2026, time.August, 2, 3, 4, 5, 0, time.UTC),
			LastError: "lifecycle-secret-token=do-not-serialize stack=raw-stack payload=raw-payload",
		},
		Status: workerruntime.PeriodicStatus{
			LastOutcome: workerruntime.OutcomeError,
			LastError:   "periodic-secret-token=do-not-serialize upstream-response=raw-content",
			RunCount:    7,
			ErrorCount:  1,
		},
	}}))

	h := NewOpsHandler(newMonitoringEnabledOpsService())
	h.SetWorkerRuntime(runtime)
	r := newOpsSystemLogTestRouter(h, true)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/status", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), "lifecycle-secret-token=do-not-serialize")
	require.NotContains(t, response.Body.String(), "periodic-secret-token=do-not-serialize")
	require.NotContains(t, response.Body.String(), "raw-stack")
	require.NotContains(t, response.Body.String(), "raw-payload")
	require.NotContains(t, response.Body.String(), "raw-content")
}

func TestOpsHandlerWorkerRuntimeStatusSanitizesPointerRuntimeStatuses(t *testing.T) {
	runtime := workerruntime.NewRuntime(workerruntime.NewRegistry())
	require.NoError(t, runtime.Register(workerRuntimeSnapshotFixture{snapshot: workerruntime.Snapshot{
		Descriptor: workerruntime.Descriptor{
			Name:             "pointer-periodic-status",
			Kind:             workerruntime.KindPeriodic,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Status: &workerruntime.PeriodicStatus{
			LastRunAt:    time.Date(2026, time.August, 2, 1, 2, 3, 0, time.UTC),
			NextRunAt:    time.Date(2026, time.August, 2, 1, 3, 3, 0, time.UTC),
			LastDuration: 1500 * time.Millisecond,
			LastOutcome:  workerruntime.OutcomeError,
			LastError:    "pointer-periodic-secret=do-not-serialize",
			RunCount:     11,
			SuccessCount: 7,
			ErrorCount:   2,
			PanicCount:   1,
			TimeoutCount: 1,
			StillRunning: true,
		},
	}}))
	require.NoError(t, runtime.Register(workerRuntimeSnapshotFixture{snapshot: workerruntime.Snapshot{
		Descriptor: workerruntime.Descriptor{
			Name:             "pointer-pool-status",
			Kind:             workerruntime.KindPool,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Status: &workerruntime.PoolStatus{
			Accepting:          true,
			StillRunning:       true,
			MaxConcurrency:     8,
			RunningWorkers:     3,
			WaitingTasks:       5,
			SubmittedTasks:     21,
			CompletedTasks:     16,
			SuccessfulTasks:    14,
			FailedTasks:        2,
			DroppedTasks:       1,
			DroppedQueueFull:   1,
			DroppedPoolStopped: 2,
			SyncFallbackTasks:  4,
		},
	}}))
	h := NewOpsHandler(newMonitoringEnabledOpsService())
	h.SetWorkerRuntime(runtime)
	r := newOpsSystemLogTestRouter(h, true)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/status", nil))

	require.Equal(t, http.StatusOK, response.Code)
	require.NotContains(t, response.Body.String(), "pointer-periodic-secret=do-not-serialize")

	var body struct {
		Data struct {
			Workers []struct {
				Descriptor struct {
					Name string `json:"Name"`
				} `json:"Descriptor"`
				Status json.RawMessage `json:"Status"`
			} `json:"workers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Len(t, body.Data.Workers, 2)

	statuses := make(map[string]map[string]any, len(body.Data.Workers))
	for _, worker := range body.Data.Workers {
		var status map[string]any
		require.NoError(t, json.Unmarshal(worker.Status, &status))
		statuses[worker.Descriptor.Name] = status
	}

	periodic := statuses["pointer-periodic-status"]
	require.Contains(t, periodic, "LastRunAt")
	require.Contains(t, periodic, "NextRunAt")
	require.Equal(t, float64((1500 * time.Millisecond).Nanoseconds()), periodic["LastDuration"])
	require.Equal(t, string(workerruntime.OutcomeError), periodic["LastOutcome"])
	require.Equal(t, float64(11), periodic["RunCount"])
	require.Equal(t, float64(7), periodic["SuccessCount"])
	require.Equal(t, float64(2), periodic["ErrorCount"])
	require.Equal(t, float64(1), periodic["PanicCount"])
	require.Equal(t, float64(1), periodic["TimeoutCount"])
	require.Equal(t, true, periodic["StillRunning"])
	require.NotContains(t, periodic, "LastError")

	pool := statuses["pointer-pool-status"]
	require.Equal(t, true, pool["Accepting"])
	require.Equal(t, true, pool["StillRunning"])
	require.Equal(t, float64(8), pool["MaxConcurrency"])
	require.Equal(t, float64(3), pool["RunningWorkers"])
	require.Equal(t, float64(5), pool["WaitingTasks"])
	require.Equal(t, float64(21), pool["SubmittedTasks"])
	require.Equal(t, float64(16), pool["CompletedTasks"])
	require.Equal(t, float64(14), pool["SuccessfulTasks"])
	require.Equal(t, float64(2), pool["FailedTasks"])
	require.Equal(t, float64(1), pool["DroppedTasks"])
	require.Equal(t, float64(1), pool["DroppedQueueFull"])
	require.Equal(t, float64(2), pool["DroppedPoolStopped"])
	require.Equal(t, float64(4), pool["SyncFallbackTasks"])
}

func TestProjectWorkerRuntimeSnapshotPreservesNilPointerStatus(t *testing.T) {
	var nilStatus *workerruntime.PeriodicStatus
	projected := projectWorkerRuntimeSnapshot(workerruntime.Snapshot{Status: nilStatus})
	require.Nil(t, projected.Status)
}

func TestOpsHandlerWorkerRuntimeStatusReturnsProcessLocalSnapshots(t *testing.T) {
	runtime := newStartedRuntimeForHandler(t)
	h := NewOpsHandler(newMonitoringEnabledOpsService())
	h.SetWorkerRuntime(runtime)
	r := newOpsSystemLogTestRouter(h, true)

	response := httptest.NewRecorder()
	r.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/workers/status", nil))

	require.Equal(t, http.StatusOK, response.Code)

	var body struct {
		Code int `json:"code"`
		Data struct {
			Scope   string            `json:"scope"`
			Workers []json.RawMessage `json:"workers"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, 0, body.Code)
	require.Equal(t, "process", body.Data.Scope)
	require.Len(t, body.Data.Workers, 1)

	var worker struct {
		Descriptor struct {
			Name string `json:"Name"`
			Kind string `json:"Kind"`
		} `json:"Descriptor"`
		Lifecycle struct {
			State string `json:"State"`
		} `json:"Lifecycle"`
	}
	require.NoError(t, json.Unmarshal(body.Data.Workers[0], &worker))
	require.Equal(t, "worker-status-test", worker.Descriptor.Name)
	require.Equal(t, "periodic", worker.Descriptor.Kind)
	require.Equal(t, "running", worker.Lifecycle.State)
	require.NotContains(t, response.Body.String(), "stack")
	require.NotContains(t, response.Body.String(), "payload")
	require.NotContains(t, response.Body.String(), "pid")
}

type workerRuntimeSnapshotFixture struct {
	snapshot workerruntime.Snapshot
}

func (f workerRuntimeSnapshotFixture) Descriptor() workerruntime.Descriptor {
	return f.snapshot.Descriptor
}
func (f workerRuntimeSnapshotFixture) Start(context.Context) error      { return nil }
func (f workerRuntimeSnapshotFixture) Stop(context.Context) error       { return nil }
func (f workerRuntimeSnapshotFixture) Snapshot() workerruntime.Snapshot { return f.snapshot }

func newMonitoringEnabledOpsService() *service.OpsService {
	return service.NewOpsService(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
}

func newStartedRuntimeForHandler(t *testing.T) *workerruntime.Runtime {
	t.Helper()

	worker, err := workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "worker-status-test",
			Kind:             workerruntime.KindPeriodic,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval: time.Hour,
		Timeout:  time.Second,
		Run: func(context.Context) error {
			return nil
		},
	})
	require.NoError(t, err)

	runtime := workerruntime.NewRuntime(workerruntime.NewRegistry())
	require.NoError(t, runtime.Register(worker))
	require.NoError(t, runtime.StartAll(context.Background()))
	t.Cleanup(func() { _, _ = runtime.StopAll(context.Background()) })
	return runtime
}
