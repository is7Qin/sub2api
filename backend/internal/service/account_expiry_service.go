package service

import (
	"context"
	"log"
	"time"
)

// AccountExpiryService pauses expired accounts when auto-pause is enabled.
type AccountExpiryService struct {
	accountRepo AccountRepository
	interval    time.Duration
}

func NewAccountExpiryService(accountRepo AccountRepository, interval time.Duration) *AccountExpiryService {
	return &AccountExpiryService{
		accountRepo: accountRepo,
		interval:    interval,
	}
}

// Interval returns the configured worker interval.
func (s *AccountExpiryService) Interval() time.Duration {
	if s == nil {
		return 0
	}
	return s.interval
}

// Start is retained as a no-op while runtime lifecycle ownership is introduced.
func (s *AccountExpiryService) Start() {}

// Run pauses expired accounts once.
func (s *AccountExpiryService) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	updated, err := s.accountRepo.AutoPauseExpiredAccounts(ctx, time.Now())
	if err != nil {
		log.Printf("[AccountExpiry] Auto pause expired accounts failed: %v", err)
		return err
	}
	if updated > 0 {
		log.Printf("[AccountExpiry] Auto paused %d expired accounts", updated)
	}
	return nil
}
