package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

// TempUnscheduler 用于 HandleFailoverError 中同账号重试耗尽后的临时封禁。
// GatewayService 隐式实现此接口。
type TempUnscheduler interface {
	TempUnscheduleRetryableError(ctx context.Context, accountID int64, failoverErr *service.UpstreamFailoverError)
}

// FailoverAction 表示 failover 错误处理后的下一步动作
type FailoverAction int

const (
	// FailoverContinue 继续循环（同账号重试或切换账号，调用方统一 continue）
	FailoverContinue FailoverAction = iota
	// FailoverExhausted 切换次数耗尽（调用方应返回错误响应）
	FailoverExhausted
	// FailoverCanceled context 已取消（调用方应直接 return）
	FailoverCanceled
	// FailoverDirectReturn recognized policy owns the safe client presentation.
	FailoverDirectReturn
)

const (
	// maxSameAccountRetries 同账号默认重试次数（针对 RetryableOnSameAccount 错误）
	maxSameAccountRetries = 3
	// sameAccountRetryDelay 同账号重试间隔
	sameAccountRetryDelay = 500 * time.Millisecond
	// singleAccountBackoffDelay 单账号分组 503 退避重试固定延时。
	// Service 层在 SingleAccountRetry 模式下已做充分原地重试（最多 3 次、总等待 30s），
	// Handler 层只需短暂间隔后重新进入 Service 层即可。
	singleAccountBackoffDelay = 2 * time.Second
)

// FailoverState 跨循环迭代共享的 failover 状态
type UpstreamRecoveryState struct {
	sameAccountRetryBudget int
	sameAccountRetriesUsed int
	transitionBudget       int
	transitionsUsed        int
	hasRecoveryPolicy      bool
	bestCandidate          *service.UpstreamErrorCandidate
}

func NewUpstreamRecoveryState() *UpstreamRecoveryState {
	return &UpstreamRecoveryState{}
}

func (s *UpstreamRecoveryState) AdoptPolicy(policy service.UpstreamRecoveryPolicy) {
	if s == nil {
		return
	}
	if !s.hasRecoveryPolicy {
		s.sameAccountRetryBudget = policy.SameAccountRetryBudget
		s.transitionBudget = policy.AccountTransitionBudget
		s.hasRecoveryPolicy = true
		return
	}
	if policy.SameAccountRetryBudget < s.sameAccountRetryBudget {
		s.sameAccountRetryBudget = policy.SameAccountRetryBudget
	}
	if policy.AccountTransitionBudget < s.transitionBudget {
		s.transitionBudget = policy.AccountTransitionBudget
	}
}

// ObserveFailoverError retains only bounded client presentation data and, when
// available, adopts the request-wide recovery budget attached to the fact.
func (s *UpstreamRecoveryState) ObserveFailoverError(failoverErr *service.UpstreamFailoverError) (service.UpstreamRecoveryPolicy, bool) {
	if s == nil || failoverErr == nil {
		return service.UpstreamRecoveryPolicy{}, false
	}
	if fact, ok := failoverErr.UpstreamFact(); ok {
		if policy, recognized := service.ResolveUpstreamRecoveryPolicy(fact); recognized {
			if policy.Disposition == service.UpstreamAttemptDirectReturn {
				return policy, true
			}
			s.AdoptPolicy(policy)
			s.RetainCandidate(service.NewUpstreamErrorCandidate(fact, policy.CandidateRank))
			return policy, true
		}
		s.RetainCandidate(service.NewUpstreamErrorCandidate(fact, service.UpstreamCandidateStatusOnly))
		return service.UpstreamRecoveryPolicy{}, false
	}
	s.RetainCandidate(service.NewUpstreamErrorCandidate(service.UpstreamErrorFact{
		HTTPStatusKnown: failoverErr.StatusCode > 0,
		HTTPStatus:      failoverErr.StatusCode,
	}, service.UpstreamCandidateStatusOnly))
	return service.UpstreamRecoveryPolicy{}, false
}

func (s *UpstreamRecoveryState) CanRetrySameAccount() bool {
	return s != nil && s.sameAccountRetriesUsed < s.sameAccountRetryBudget
}

func (s *UpstreamRecoveryState) RecordSameAccountRetry() {
	if s != nil {
		s.sameAccountRetriesUsed++
	}
}

func (s *UpstreamRecoveryState) HasTransitionBudget() bool {
	return s != nil && s.hasRecoveryPolicy
}

func (s *UpstreamRecoveryState) RetainCandidate(candidate *service.UpstreamErrorCandidate) {
	if s == nil || candidate == nil {
		return
	}
	if s.bestCandidate == nil || candidate.Rank >= s.bestCandidate.Rank {
		copy := *candidate
		s.bestCandidate = &copy
	}
}

func (s *UpstreamRecoveryState) CanTransition(configuredHardCap int) bool {
	return s != nil && configuredHardCap > 0 &&
		s.transitionsUsed < configuredHardCap &&
		s.transitionBudget > s.transitionsUsed
}

func (s *UpstreamRecoveryState) RecordTransition() {
	if s != nil {
		s.transitionsUsed++
	}
}

func (s *UpstreamRecoveryState) ClearOnSuccess() {
	if s == nil {
		return
	}
	s.bestCandidate = nil
	s.sameAccountRetryBudget = 0
	s.sameAccountRetriesUsed = 0
	s.transitionBudget = 0
	s.transitionsUsed = 0
	s.hasRecoveryPolicy = false
}

func (s *UpstreamRecoveryState) FinalCandidate() (*service.UpstreamErrorCandidate, bool) {
	if s == nil || s.bestCandidate == nil {
		return nil, false
	}
	copy := *s.bestCandidate
	return &copy, true
}

// directReturnCandidate renders the current fact without retaining it in
// request-wide failover candidate state.
func directReturnCandidate(failoverErr *service.UpstreamFailoverError) (*service.UpstreamErrorCandidate, bool) {
	if failoverErr == nil {
		return nil, false
	}
	fact, ok := failoverErr.UpstreamFact()
	if !ok {
		return nil, false
	}
	policy, recognized := service.ResolveUpstreamRecoveryPolicy(fact)
	if !recognized || policy.Disposition != service.UpstreamAttemptDirectReturn {
		return nil, false
	}
	return service.NewUpstreamErrorCandidate(fact, policy.CandidateRank), true
}

// openAIDirectReturnCandidate mirrors the OpenAI handler's direct-return
// boundary while keeping the fact out of request-wide failover retention.
func openAIDirectReturnCandidate(_ *UpstreamRecoveryState, failoverErr *service.UpstreamFailoverError) (*service.UpstreamErrorCandidate, bool) {
	return directReturnCandidate(failoverErr)
}

type FailoverState struct {
	SwitchCount           int
	MaxSwitches           int
	FailedAccountIDs      map[int64]struct{}
	SameAccountRetryCount map[int64]int
	LastFailoverErr       *service.UpstreamFailoverError
	ForceCacheBilling     bool
	Recovery              *UpstreamRecoveryState
	hasBoundSession       bool
}

func (s *FailoverState) FinalCandidate() (*service.UpstreamErrorCandidate, bool) {
	if s == nil || s.Recovery == nil {
		return nil, false
	}
	return s.Recovery.FinalCandidate()
}

// Candidate presentation may normalize the client status; ops must retain only
// the status actually observed from upstream.
func setOpsUpstreamCandidateError(c *gin.Context, fact service.UpstreamErrorFact, message string) {
	if !fact.HTTPStatusKnown {
		c.Set(service.OpsUpstreamStatusCodeKey, 0)
		service.SetOpsUpstreamError(c, 0, message, "")
		return
	}
	service.SetOpsUpstreamError(c, fact.HTTPStatus, message, "")
}

// NewFailoverState 创建 failover 状态
func NewFailoverState(maxSwitches int, hasBoundSession bool) *FailoverState {
	return &FailoverState{
		MaxSwitches:           maxSwitches,
		FailedAccountIDs:      make(map[int64]struct{}),
		SameAccountRetryCount: make(map[int64]int),
		Recovery:              NewUpstreamRecoveryState(),
		hasBoundSession:       hasBoundSession,
	}
}

// HandleFailoverError 处理 UpstreamFailoverError，返回下一步动作。
// 包含：缓存计费判断、同账号重试、临时封禁、切换计数、Antigravity 延时。
func (s *FailoverState) HandleFailoverError(
	ctx context.Context,
	gatewayService TempUnscheduler,
	accountID int64,
	platform string,
	retryLimit int,
	failoverErr *service.UpstreamFailoverError,
) FailoverAction {
	if ctx != nil && ctx.Err() != nil {
		return FailoverCanceled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if failoverErr == nil {
		return FailoverExhausted
	}

	if ctx.Err() != nil {
		return FailoverCanceled
	}
	policy, recognized := s.Recovery.ObserveFailoverError(failoverErr)
	if recognized && policy.Disposition == service.UpstreamAttemptDirectReturn {
		return FailoverDirectReturn
	}
	s.LastFailoverErr = failoverErr

	// 缓存计费判断
	if needForceCacheBilling(s.hasBoundSession, failoverErr) {
		if ctx.Err() != nil {
			return FailoverCanceled
		}
		s.ForceCacheBilling = true
	}

	// Fact-backed policies cap configured retries request-wide; factless legacy
	// carriers retain their established per-account configured behavior.
	canRetrySameAccount := failoverErr.RetryableOnSameAccount && s.SameAccountRetryCount[accountID] < retryLimit
	if recognized {
		canRetrySameAccount = canRetrySameAccount && s.Recovery.CanRetrySameAccount()
	}
	if canRetrySameAccount {
		if ctx.Err() != nil {
			return FailoverCanceled
		}
		s.SameAccountRetryCount[accountID]++
		if recognized {
			s.Recovery.RecordSameAccountRetry()
		}
		logger.FromContext(ctx).Warn("gateway.failover_same_account_retry",
			zap.Int64("account_id", accountID),
			zap.Int("upstream_status", failoverErr.StatusCode),
			zap.Int("same_account_retry_count", s.SameAccountRetryCount[accountID]),
			zap.Int("same_account_retry_max", retryLimit),
		)
		if !sleepWithContext(ctx, sameAccountRetryDelay) ||
			ctx.Err() != nil {
			return FailoverCanceled
		}
		return FailoverContinue
	}

	// 同账号重试用尽，执行临时封禁
	if failoverErr.RetryableOnSameAccount {
		if ctx.Err() != nil {
			return FailoverCanceled
		}
		gatewayService.TempUnscheduleRetryableError(ctx, accountID, failoverErr)
	}

	// 加入失败列表
	if ctx.Err() != nil {
		return FailoverCanceled
	}
	s.FailedAccountIDs[accountID] = struct{}{}

	// Recognized recoverable policies own a request-wide transition budget. Legacy
	// failover errors retain their configured behavior until their boundary is migrated.
	if s.Recovery.HasTransitionBudget() && !s.Recovery.CanTransition(s.MaxSwitches) {
		return FailoverExhausted
	}

	// 检查是否耗尽
	if s.SwitchCount >= s.MaxSwitches {
		return FailoverExhausted
	}

	// 递增切换计数
	if ctx.Err() != nil {
		return FailoverCanceled
	}
	s.SwitchCount++
	if s.Recovery.HasTransitionBudget() {
		s.Recovery.RecordTransition()
	}
	logger.FromContext(ctx).Warn("gateway.failover_switch_account",
		zap.Int64("account_id", accountID),
		zap.Int("upstream_status", failoverErr.StatusCode),
		zap.Int("switch_count", s.SwitchCount),
		zap.Int("max_switches", s.MaxSwitches),
	)

	// Antigravity 平台换号线性递增延时
	if platform == service.PlatformAntigravity {
		delay := time.Duration(s.SwitchCount-1) * time.Second
		if !sleepWithContext(ctx, delay) ||
			ctx.Err() != nil {
			return FailoverCanceled
		}
	}

	if ctx.Err() != nil {
		return FailoverCanceled
	}
	return FailoverContinue
}

// HandleSelectionExhausted 处理选号失败（所有候选账号都在排除列表中）时的退避重试决策。
// 针对 Antigravity 单账号分组的 503 (MODEL_CAPACITY_EXHAUSTED) 场景：
// 清除排除列表、等待退避后重新选号。
//
// 返回 FailoverContinue 时，调用方应设置 SingleAccountRetry context 并 continue。
// 返回 FailoverExhausted 时，调用方应返回错误响应。
// 返回 FailoverCanceled 时，调用方应直接 return。
func (s *FailoverState) HandleSelectionExhausted(ctx context.Context) FailoverAction {
	if ctx != nil && ctx.Err() != nil {
		return FailoverCanceled
	}

	if s.Recovery != nil && s.Recovery.HasTransitionBudget() &&
		!s.Recovery.CanTransition(s.MaxSwitches) {
		return FailoverExhausted
	}

	if s.LastFailoverErr != nil &&
		s.LastFailoverErr.StatusCode == http.StatusServiceUnavailable &&
		s.SwitchCount <= s.MaxSwitches {

		logger.FromContext(ctx).Warn("gateway.failover_single_account_backoff",
			zap.Duration("backoff_delay", singleAccountBackoffDelay),
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		if !sleepWithContext(ctx, singleAccountBackoffDelay) ||
			ctx.Err() != nil {
			return FailoverCanceled
		}
		logger.FromContext(ctx).Warn("gateway.failover_single_account_retry",
			zap.Int("switch_count", s.SwitchCount),
			zap.Int("max_switches", s.MaxSwitches),
		)
		if ctx.Err() != nil {
			return FailoverCanceled
		}
		s.FailedAccountIDs = make(map[int64]struct{})
		return FailoverContinue
	}
	return FailoverExhausted
}

// needForceCacheBilling 判断 failover 时是否需要强制缓存计费。
// 粘性会话切换账号、或上游明确标记时，将 input_tokens 转为 cache_read 计费。
func needForceCacheBilling(hasBoundSession bool, failoverErr *service.UpstreamFailoverError) bool {
	return hasBoundSession || (failoverErr != nil && failoverErr.ForceCacheBilling)
}

// failoverClientGone stops HTTP failover after the downstream client disconnects.
// The active detached upstream attempt still completes for draining and accounting.
func failoverClientGone(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Context().Err() == nil {
		return false
	}
	if service.StopOpenAICompactSSEKeepaliveCommitted(c) {
		return true
	}
	if c.Writer != nil && !c.Writer.Written() {
		c.Status(statusClientClosedRequest)
	}
	return true
}

// handleHTTPAttemptNotAdmitted must be the first forward-error check. The
// service still owns no attempt, so only downstream cancellation semantics and
// the handler-owned slot release apply.
func handleHTTPAttemptNotAdmitted(c *gin.Context, err error) bool {
	if !service.IsHTTPUpstreamAttemptNotAdmitted(err) {
		return false
	}
	failoverClientGone(c)
	return true
}

// selectFailoverAccount keeps client cancellation ahead of routing-failure
// attribution at every HTTP account-selection boundary.
func selectFailoverAccount(c *gin.Context, selectAccount func() (*service.AccountSelectionResult, error)) (*service.AccountSelectionResult, error, bool) {
	if failoverClientGone(c) {
		return nil, nil, true
	}
	selection, err := selectAccount()
	releaseSelection := func() {
		if selection != nil && selection.ReleaseFunc != nil {
			release := selection.ReleaseFunc
			selection.ReleaseFunc = nil
			release()
		}
	}
	if failoverClientGone(c) {
		releaseSelection()
		return nil, err, true
	}
	if err != nil {
		releaseSelection()
		return nil, err, false
	}
	if selection == nil || selection.Account == nil {
		releaseSelection()
		return nil, service.ErrNoAvailableAccounts, false
	}
	return selection, nil, false
}

// sleepWithContext 等待指定时长，返回 false 表示 context 已取消。
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
