package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	billingOutboxBatchSize    = 100
	billingOutboxPollInterval = 500 * time.Millisecond
	// Apply is bounded within the lease. Finalization may block indefinitely, so
	// its claim is renewed until the synchronous Finalize call returns.
	billingOutboxLease                     = 90 * time.Second
	billingOutboxConcurrency               = 16
	billingOutboxApplyTimeout              = 30 * time.Second
	billingOutboxAckRetryTimeout           = 2 * time.Second
	billingOutboxFinalizationRenewInterval = 20 * time.Second
	billingOutboxFinalizationDBTimeout     = 2 * time.Second
	billingOutboxMaxAttempts               = 10
)

// BillingOutboxHealth reports durable backlog and in-process replay state.
type BillingOutboxHealth struct {
	Running     bool          `json:"running"`
	Processed   uint64        `json:"processed"`
	Failures    uint64        `json:"failures"`
	Pending     int64         `json:"pending"`
	Processing  int64         `json:"processing"`
	Terminal    int64         `json:"terminal"`
	OldestLag   time.Duration `json:"oldest_lag"`
	LastError   string        `json:"last_error,omitempty"`
	StatsError  string        `json:"stats_error,omitempty"`
	MaxAttempts int           `json:"max_attempts"`
}

// BillingOutboxPostProcessor replays non-transactional enforcement updates
// only after a newly applied billing command commits successfully.
type BillingOutboxPostProcessor interface {
	Finalize(ctx context.Context, command *BillingOutboxCommand, result *UsageBillingApplyResult) error
}

type BillingOutboxWorker struct {
	repo                           BillingOutboxRepository
	billing                        UsageBillingRepository
	postProcessor                  BillingOutboxPostProcessor
	workerID                       string
	finalizationLease              time.Duration
	finalizationLeaseRenewInterval time.Duration
	finalizationDBTimeout          time.Duration
	ctx                            context.Context
	cancel                         context.CancelFunc
	wg                             sync.WaitGroup
	start                          sync.Once
	stop                           sync.Once
	running                        atomic.Bool
	processed                      atomic.Uint64
	failures                       atomic.Uint64
	lastError                      atomic.Value
}

func NewBillingOutboxWorker(repo BillingOutboxRepository, billing UsageBillingRepository, postProcessor ...BillingOutboxPostProcessor) *BillingOutboxWorker {
	ctx, cancel := context.WithCancel(context.Background())
	var processor BillingOutboxPostProcessor
	if len(postProcessor) > 0 {
		processor = postProcessor[0]
	}
	worker := &BillingOutboxWorker{
		repo: repo, billing: billing, postProcessor: processor, workerID: uuid.NewString(),
		finalizationLease: billingOutboxLease, finalizationLeaseRenewInterval: billingOutboxFinalizationRenewInterval,
		finalizationDBTimeout: billingOutboxFinalizationDBTimeout, ctx: ctx, cancel: cancel,
	}
	worker.lastError.Store("")
	return worker
}

func (w *BillingOutboxWorker) Start() {
	if w == nil || w.repo == nil || w.billing == nil {
		return
	}
	w.start.Do(func() {
		w.running.Store(true)
		w.wg.Add(1)
		go w.run()
	})
}

func (w *BillingOutboxWorker) Stop() {
	if w == nil {
		return
	}
	w.stop.Do(func() {
		w.cancel()
		w.wg.Wait()
		w.running.Store(false)
	})
}

func (w *BillingOutboxWorker) run() {
	defer w.wg.Done()
	defer w.running.Store(false)
	ticker := time.NewTicker(billingOutboxPollInterval)
	defer ticker.Stop()
	for {
		if err := w.processBatch(w.ctx); err != nil && w.ctx.Err() == nil {
			w.recordFailure(err)
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *BillingOutboxWorker) processBatch(ctx context.Context) error {
	if finalRepo, ok := w.repo.(BillingOutboxFinalizationRepository); ok {
		finalRecords, err := finalRepo.ClaimFinalization(ctx, w.workerID, billingOutboxConcurrency, w.finalizationLease)
		if err != nil {
			return fmt.Errorf("claim billing outbox finalizations: %w", err)
		}
		if err := w.processFinalizationBatch(ctx, finalRecords, finalRepo); err != nil {
			return err
		}
	}
	records, err := w.repo.Claim(ctx, w.workerID, billingOutboxConcurrency, billingOutboxLease)
	if err != nil {
		return fmt.Errorf("claim billing outbox commands: %w", err)
	}
	if err := w.processApplyBatch(ctx, records); err != nil {
		return err
	}
	return nil
}

func (w *BillingOutboxWorker) processApplyBatch(ctx context.Context, records []BillingOutboxRecord) error {
	semaphore := make(chan struct{}, billingOutboxConcurrency)
	var wg sync.WaitGroup
	for i := range records {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(record BillingOutboxRecord) {
			defer wg.Done()
			defer func() { <-semaphore }()
			w.processRecord(ctx, record)
		}(records[i])
	}
	wg.Wait()
	return nil
}

func (w *BillingOutboxWorker) processFinalizationBatch(ctx context.Context, records []BillingOutboxRecord, repo BillingOutboxFinalizationRepository) error {
	semaphore := make(chan struct{}, billingOutboxConcurrency)
	var wg sync.WaitGroup
	for i := range records {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case semaphore <- struct{}{}:
		}
		wg.Add(1)
		go func(record BillingOutboxRecord) {
			defer wg.Done()
			defer func() { <-semaphore }()
			w.processFinalization(ctx, record, repo)
		}(records[i])
	}
	wg.Wait()
	return nil
}

func (w *BillingOutboxWorker) processRecord(parent context.Context, record BillingOutboxRecord) {
	command := record.Command
	command.Normalize()
	if err := command.Validate(); err != nil {
		w.recordFailure(err)
		w.persistFailure(record, err, true)
		return
	}

	applyCtx, applyCancel := context.WithTimeout(parent, billingOutboxApplyTimeout)
	var result *UsageBillingApplyResult
	var err error
	if staged, ok := w.billing.(UsageBillingFinalizationRepository); ok {
		result, err = staged.ApplyAndStageOutboxFinalization(applyCtx, &command.Billing, UsageBillingOutboxBinding{OutboxID: record.ID, WorkerID: w.workerID})
		applyCancel()
		if err != nil {
			terminal := billingOutboxIsTerminalError(err)
			w.recordFailure(err)
			w.persistFailure(record, err, terminal)
			return
		}
		if result == nil || result.Applied {
			// Staging atomically records the Apply outcome and transfers ownership
			// to the finalization phase. A later claim performs post-effects.
			return
		}
		// A pre-existing deduplication key means this row did not own unfinished
		// finalization, so it can complete without post-effects.
		ackCtx, ackCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
		ackErr := w.repo.Ack(ackCtx, record.ID, w.workerID)
		ackCancel()
		if ackErr != nil {
			w.recordFailure(fmt.Errorf("ack billing outbox command %d: %w", record.ID, ackErr))
			return
		}
		w.processed.Add(1)
		w.lastError.Store("")
		return
	}
	applyCancel()
	// Monetary Apply and the durable finalization handoff must be one repository
	// operation. A legacy repository cannot safely acknowledge this command: it
	// could commit money and lose the retryable post-effect route.
	err = ErrBillingOutboxFinalizationUnsupported
	w.recordFailure(err)
	w.persistFailure(record, err, false)
}

func (w *BillingOutboxWorker) processFinalization(parent context.Context, record BillingOutboxRecord, repo BillingOutboxFinalizationRepository) {
	finalizeCtx, cancelFinalize := context.WithCancel(parent)
	defer cancelFinalize()
	ownershipLost := func() bool { return false }

	command := record.Command
	command.Normalize()
	if err := command.Validate(); err != nil {
		w.recordFailure(err)
		w.persistFinalizationFailure(repo, record, err, true, ownershipLost)
		return
	}
	if record.ApplyResult == nil {
		err := errors.New("billing outbox finalization result is missing")
		w.recordFailure(err)
		w.persistFinalizationFailure(repo, record, err, true, ownershipLost)
		return
	}
	if w.postProcessor != nil {
		stopHeartbeat, heartbeatLost := w.renewFinalizationLease(cancelFinalize, record, repo)
		err := w.postProcessor.Finalize(finalizeCtx, &command, record.ApplyResult)
		stopHeartbeat()
		ownershipLost = heartbeatLost
		if err != nil {
			w.recordFailure(fmt.Errorf("finalize billing outbox command %d: %w", record.ID, err))
			w.persistFinalizationFailure(repo, record, err, billingOutboxIsTerminalError(err), ownershipLost)
			return
		}
	}
	if ownershipLost() || !w.renewFinalizationLeaseOnce(repo, record.ID) {
		w.recordFailure(fmt.Errorf("ack billing outbox finalization %d: %w", record.ID, ErrBillingOutboxClaimLost))
		return
	}
	ackCtx, cancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	err := repo.AckFinalization(ackCtx, record.ID, w.workerID)
	cancel()
	if err != nil {
		w.recordFailure(fmt.Errorf("ack billing outbox finalization %d: %w", record.ID, err))
		return
	}
	w.processed.Add(1)
	w.lastError.Store("")
}

// renewFinalizationLease fences finalization state transitions while Finalize is
// blocked. It intentionally uses background-bounded contexts: a stopped parent
// must not interrupt the final ownership check needed to prevent a stale ACK.
func (w *BillingOutboxWorker) renewFinalizationLease(cancelFinalize context.CancelFunc, record BillingOutboxRecord, repo BillingOutboxFinalizationRepository) (func(), func() bool) {
	var lost atomic.Bool
	stop := make(chan struct{})
	done := make(chan struct{})
	interval := w.finalizationLeaseRenewInterval
	if interval <= 0 || interval >= w.finalizationLease {
		interval = w.finalizationLease / 3
	}
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if !w.renewFinalizationLeaseOnce(repo, record.ID) {
					lost.Store(true)
					cancelFinalize()
				}
			}
		}
	}()
	return func() { close(stop); <-done }, lost.Load
}

func (w *BillingOutboxWorker) renewFinalizationLeaseOnce(repo BillingOutboxFinalizationRepository, id int64) bool {
	timeout := w.finalizationDBTimeout
	if timeout <= 0 {
		timeout = billingOutboxFinalizationDBTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	err := repo.RenewFinalizationLease(ctx, id, w.workerID, w.finalizationLease)
	cancel()
	return err == nil
}

func (w *BillingOutboxWorker) persistFinalizationFailure(repo BillingOutboxFinalizationRepository, record BillingOutboxRecord, err error, terminal bool, ownershipLost func() bool) {
	if ownershipLost() || !w.renewFinalizationLeaseOnce(repo, record.ID) {
		w.recordFailure(fmt.Errorf("release billing outbox finalization %d: %w", record.ID, ErrBillingOutboxClaimLost))
		return
	}
	// Monetary effects already committed before this phase. A transient finalizer
	// failure must remain replayable regardless of the ordinary Apply retry limit.
	retryAt := time.Now().UTC().Add(billingOutboxRetryDelay(record.Attempts + 1))
	if terminal {
		retryAt = time.Time{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	retryErr := repo.RetryFinalization(ctx, record.ID, w.workerID, retryAt, boundedBillingOutboxWorkerError(err), terminal)
	cancel()
	if retryErr != nil {
		w.recordFailure(fmt.Errorf("release billing outbox finalization %d: %w", record.ID, retryErr))
	}
}

func (w *BillingOutboxWorker) persistFailure(record BillingOutboxRecord, err error, terminal bool) {
	// Retryable infrastructure failures remain recoverable indefinitely; the
	// capped backoff limits poll delay without discarding durable work.
	retryAt := time.Now().UTC().Add(billingOutboxRetryDelay(record.Attempts + 1))
	if terminal {
		retryAt = time.Time{}
	}
	retryCtx, retryCancel := context.WithTimeout(context.Background(), billingOutboxAckRetryTimeout)
	retryErr := w.repo.Retry(retryCtx, record.ID, w.workerID, retryAt, boundedBillingOutboxWorkerError(err), terminal)
	retryCancel()
	if retryErr != nil {
		w.recordFailure(fmt.Errorf("release billing outbox command %d: %w", record.ID, retryErr))
	}
}

func billingOutboxIsTerminalError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// Application errors with client-side status codes are deterministic poison
	// commands; retry only infrastructure/server failures.
	if code := infraerrors.Code(err); code >= 400 && code < 500 {
		return true
	}
	for _, terminalErr := range []error{
		ErrBillingOutboxAttemptIDRequired,
		ErrBillingOutboxAPIKeyRequired,
		ErrBillingOutboxFingerprintConflict,
		ErrUsageBillingRequestIDRequired,
		ErrUsageBillingRequestConflict,
		ErrUsageLogAccountRequired,
		ErrAccountNotFound,
		ErrUserNotFound,
		ErrAPIKeyNotFound,
		ErrSubscriptionNotFound,
	} {
		if errors.Is(err, terminalErr) {
			return true
		}
	}
	return false
}

func billingOutboxRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 9 {
		attempt = 9
	}
	base := time.Second * time.Duration(1<<(attempt-1))
	return time.Duration(float64(base) * (0.8 + rand.Float64()*0.4))
}

func boundedBillingOutboxWorkerError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.TrimSpace(err.Error())
	message = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, message)
	if len(message) > BillingOutboxLastErrorLimit {
		return message[:BillingOutboxLastErrorLimit]
	}
	return message
}

func (w *BillingOutboxWorker) recordFailure(err error) {
	if err == nil {
		return
	}
	message := boundedBillingOutboxWorkerError(err)
	w.failures.Add(1)
	w.lastError.Store(message)
	slog.Warn("billing outbox processing failed", "error", message)
}

func (w *BillingOutboxWorker) Health(ctx context.Context) BillingOutboxHealth {
	health := BillingOutboxHealth{}
	if w == nil {
		return health
	}
	health.Running = w.running.Load()
	health.Processed = w.processed.Load()
	health.Failures = w.failures.Load()
	if value := w.lastError.Load(); value != nil {
		health.LastError, _ = value.(string)
	}
	if w.repo == nil {
		return health
	}
	stats, err := w.repo.Stats(ctx)
	if err != nil {
		health.StatsError = boundedBillingOutboxWorkerError(err)
		return health
	}
	health.Pending = stats.Pending
	health.Processing = stats.Processing
	health.Terminal = stats.Terminal
	health.MaxAttempts = stats.MaxAttempts
	if health.LastError == "" && stats.LastError != "" {
		health.LastError = boundedBillingOutboxWorkerError(errors.New(stats.LastError))
	}
	if stats.OldestCreatedAt != nil {
		health.OldestLag = time.Since(*stats.OldestCreatedAt)
		if health.OldestLag < 0 {
			health.OldestLag = 0
		}
	}
	return health
}
