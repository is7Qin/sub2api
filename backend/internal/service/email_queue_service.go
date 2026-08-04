package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// Task type constants
const (
	TaskTypeVerifyCode    = "verify_code"
	TaskTypePasswordReset = "password_reset"
)

// EmailTask 邮件发送任务
type EmailTask struct {
	Email    string
	SiteName string
	TaskType string // "verify_code" or "password_reset"
	ResetURL string // Only used for password_reset task type
	Locale   string // Optional Accept-Language locale hint
}

// EmailQueueStats reports the queue state observable by the worker runtime.
type EmailQueueStats struct {
	Accepting          bool
	StillRunning       bool
	MaxConcurrency     int
	RunningWorkers     int64
	WaitingTasks       uint64
	SubmittedTasks     uint64
	CompletedTasks     uint64
	DroppedTasks       uint64
	DroppedQueueFull   uint64
	DroppedPoolStopped uint64
}

// EmailQueueService 异步邮件队列服务
type EmailQueueService struct {
	emailService *EmailService
	taskChan     chan EmailTask
	wg           sync.WaitGroup
	stopChan     chan struct{}
	workers      int

	mu       sync.RWMutex
	started  bool
	stopping bool
	stopped  bool
	stopDone chan struct{}

	runningWorkers     atomic.Int64
	submittedTasks     atomic.Uint64
	completedTasks     atomic.Uint64
	droppedQueueFull   atomic.Uint64
	droppedPoolStopped atomic.Uint64
}

// NewEmailQueueService creates the queue. The worker runtime owns worker startup.
func NewEmailQueueService(emailService *EmailService, workers int) *EmailQueueService {
	if workers <= 0 {
		workers = 3 // 默认3个工作协程
	}
	return &EmailQueueService{
		emailService: emailService,
		taskChan:     make(chan EmailTask, 100), // 缓冲100个任务
		stopChan:     make(chan struct{}),
		workers:      workers,
	}
}

// Start starts the fixed worker set once. It is called only by the runtime adapter.
func (s *EmailQueueService) Start() error {
	if s == nil {
		return fmt.Errorf("email queue is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return nil
	}
	if s.stopping || s.stopped {
		return fmt.Errorf("email queue is stopped")
	}
	s.started = true
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		s.runningWorkers.Add(1)
		go s.worker(i)
	}
	logger.LegacyPrintf("service.email_queue", "[EmailQueue] Started %d workers", s.workers)
	return nil
}

// worker 工作协程
func (s *EmailQueueService) worker(id int) {
	defer s.wg.Done()
	defer s.runningWorkers.Add(-1)
	for {
		select {
		case task := <-s.taskChan:
			s.processTask(id, task)
			s.completedTasks.Add(1)
		case <-s.stopChan:
			logger.LegacyPrintf("service.email_queue", "[EmailQueue] Worker %d stopping", id)
			return
		}
	}
}

// processTask 处理任务
func (s *EmailQueueService) processTask(workerID int, task EmailTask) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch task.TaskType {
	case TaskTypeVerifyCode:
		if err := s.emailService.SendVerifyCode(ctx, task.Email, task.SiteName, task.Locale); err != nil {
			logger.LegacyPrintf("service.email_queue", "[EmailQueue] Worker %d failed to send verify code to %s: %v", workerID, task.Email, err)
		} else {
			logger.LegacyPrintf("service.email_queue", "[EmailQueue] Worker %d sent verify code to %s", workerID, task.Email)
		}
	case TaskTypePasswordReset:
		if err := s.emailService.SendPasswordResetEmailWithCooldown(ctx, task.Email, task.SiteName, task.ResetURL, task.Locale); err != nil {
			logger.LegacyPrintf("service.email_queue", "[EmailQueue] Worker %d failed to send password reset to %s: %v", workerID, task.Email, err)
		} else {
			logger.LegacyPrintf("service.email_queue", "[EmailQueue] Worker %d sent password reset to %s", workerID, task.Email)
		}
	default:
		logger.LegacyPrintf("service.email_queue", "[EmailQueue] Worker %d unknown task type: %s", workerID, task.TaskType)
	}
}

// EnqueueVerifyCode 将验证码发送任务加入队列
func (s *EmailQueueService) EnqueueVerifyCode(email, siteName string, locale ...string) error {
	return s.enqueue(EmailTask{Email: email, SiteName: siteName, TaskType: TaskTypeVerifyCode, Locale: firstEmailLocale(locale)}, "verify code", email)
}

// EnqueuePasswordReset 将密码重置邮件任务加入队列
func (s *EmailQueueService) EnqueuePasswordReset(email, siteName, resetURL string, locale ...string) error {
	return s.enqueue(EmailTask{Email: email, SiteName: siteName, TaskType: TaskTypePasswordReset, ResetURL: resetURL, Locale: firstEmailLocale(locale)}, "password reset", email)
}

func (s *EmailQueueService) enqueue(task EmailTask, label, email string) error {
	if s == nil {
		return fmt.Errorf("email queue is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// Minimal test instances intentionally provide only taskChan; retain their
	// enqueue semantics while production queues require runtime startup.
	accepting := (s.started || s.workers == 0) && !s.stopping && !s.stopped
	if !accepting {
		s.droppedPoolStopped.Add(1)
		return fmt.Errorf("email queue is unavailable")
	}
	select {
	case s.taskChan <- task:
		s.submittedTasks.Add(1)
		logger.LegacyPrintf("service.email_queue", "[EmailQueue] Enqueued %s task for %s", label, email)
		return nil
	default:
		s.droppedQueueFull.Add(1)
		return fmt.Errorf("email queue is full")
	}
}

// Stop begins immediate shutdown and waits for workers under ctx. taskChan remains
// open so concurrent producers cannot panic during runtime-owned shutdown.
func (s *EmailQueueService) Stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.stopDone == nil {
		s.stopping = true
		s.stopDone = make(chan struct{})
		close(s.stopChan)
		done := s.stopDone
		go func() {
			s.wg.Wait()
			s.mu.Lock()
			s.stopping = false
			s.stopped = true
			s.mu.Unlock()
			logger.LegacyPrintf("service.email_queue", "%s", "[EmailQueue] All workers stopped")
			close(done)
		}()
	}
	done := s.stopDone
	s.mu.Unlock()
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// Stats returns a detached, safely observable queue snapshot.
func (s *EmailQueueService) Stats() EmailQueueStats {
	if s == nil {
		return EmailQueueStats{}
	}
	s.mu.RLock()
	started, stopping, stopped, workers := s.started, s.stopping, s.stopped, s.workers
	s.mu.RUnlock()
	return EmailQueueStats{
		Accepting:          started && !stopping && !stopped,
		StillRunning:       s.runningWorkers.Load() > 0 || stopping,
		MaxConcurrency:     workers,
		RunningWorkers:     s.runningWorkers.Load(),
		WaitingTasks:       uint64(len(s.taskChan)),
		SubmittedTasks:     s.submittedTasks.Load(),
		CompletedTasks:     s.completedTasks.Load(),
		DroppedTasks:       s.droppedQueueFull.Load() + s.droppedPoolStopped.Load(),
		DroppedQueueFull:   s.droppedQueueFull.Load(),
		DroppedPoolStopped: s.droppedPoolStopped.Load(),
	}
}
