package service

import (
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

// NewAccountExpiryWorker adapts account expiry maintenance to the worker runtime.
func NewAccountExpiryWorker(svc *AccountExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("account expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "account-expiry",
			Kind:             workerruntime.KindPeriodic,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       svc.Interval(),
		Timeout:        5 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// NewIdempotencyCleanupWorker adapts idempotency cleanup maintenance to the worker runtime.
func NewIdempotencyCleanupWorker(svc *IdempotencyCleanupService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("idempotency cleanup service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "idempotency-cleanup",
			Kind:             workerruntime.KindPeriodic,
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       svc.Interval(),
		Timeout:        10 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}
