package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const supportDecisionPublicationDocumentTTL = 24 * time.Hour

var ErrSupportDecisionNotPublished = errors.New("support decision generation was not published")

// SupportDecisionPublisher performs one synchronous publication attempt. Its
// caller owns scheduling, retries, and the lifetime of the ownership context.
type SupportDecisionPublisher struct {
	generation   SupportDecisionGenerationRepository
	source       SupportDecisionSource
	store        SupportDecisionPublicationStore
	options      SupportDecisionBuildOptions
	ttl          time.Duration
	build        func(*SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions) (*SupportDecisionTable, error)
	buildContext func(context.Context, *SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions) (*SupportDecisionTable, error)
	encode       func(*SupportDecisionTable) ([]byte, error)
	shadow       func(context.Context, *SupportDecisionConstructionSnapshot, SupportDecisionBuildOptions, *SupportDecisionTable) (uint64, error)
	now          func() time.Time
	metrics      supportDecisionPublisherMetrics
}

// NewSupportDecisionPublisher constructs the worker-only publisher without
// starting any background work.
func NewSupportDecisionPublisher(
	generation SupportDecisionGenerationRepository,
	source SupportDecisionSource,
	store SupportDecisionPublicationStore,
	cfg *config.Config,
) *SupportDecisionPublisher {
	options := SupportDecisionBuildOptions{}
	if cfg != nil {
		options.HotModels = cfg.Gateway.Scheduling.SupportDecisionHotModels
		options.OpenAIWS = cfg.Gateway.OpenAIWS
	}
	return &SupportDecisionPublisher{
		generation:   generation,
		source:       source,
		store:        store,
		options:      options,
		ttl:          supportDecisionPublicationDocumentTTL,
		build:        BuildSupportDecisionTable,
		buildContext: BuildSupportDecisionTableContext,
		encode:       EncodeSupportDecisionDocument,
		shadow:       VerifySupportDecisionShadow,
		now:          time.Now,
	}
}

// Publish builds and attempts to activate exactly one newly allocated
// generation. A nonzero generation is returned only after activation is
// confirmed; wakeup delivery remains best effort.
func (p *SupportDecisionPublisher) Publish(ctx context.Context) (uint64, error) {
	if p == nil || p.generation == nil || p.source == nil || p.store == nil || (p.build == nil && p.buildContext == nil) || p.encode == nil || p.shadow == nil {
		return 0, errors.New("support decision publisher dependencies are incomplete")
	}
	if ctx == nil {
		return 0, errors.New("support decision publisher context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	now := p.now
	if now == nil {
		now = time.Now
	}
	started := now()
	p.metrics.sequence.Add(1)
	p.metrics.attempts.Add(1)
	p.metrics.attemptStartedUnixNano.Store(started.UnixNano())
	p.metrics.active.Store(true)
	p.metrics.sequence.Add(1)
	defer func() {
		p.metrics.sequence.Add(1)
		p.metrics.active.Store(false)
		p.metrics.sequence.Add(1)
	}()

	generation, err := p.generation.NextSupportDecisionGeneration(ctx)
	if err != nil || generation == 0 {
		return 0, p.fail(SupportDecisionPublisherStageGeneration, err, "allocate support decision generation")
	}
	snapshot, err := p.source.Load(ctx)
	if err != nil {
		return 0, p.fail(SupportDecisionPublisherStageSource, err, "load support decision source")
	}
	options := p.options
	options.Generation = generation
	buildStarted := now()
	var table *SupportDecisionTable
	if p.buildContext != nil && (p.build == nil || reflect.ValueOf(p.build).Pointer() == reflect.ValueOf(BuildSupportDecisionTable).Pointer()) {
		table, err = p.buildContext(ctx, snapshot, options)
	} else {
		table, err = p.build(snapshot, options)
	}
	p.metrics.buildDurationNanos.Store(int64(now().Sub(buildStarted)))
	if err != nil || table == nil || !table.verified || table.Generation != generation {
		return 0, p.fail(SupportDecisionPublisherStageBuild, err, "build support decision document")
	}
	shadowStarted := now()
	checks, err := p.shadow(ctx, snapshot, options, table)
	p.metrics.shadowDurationNanos.Store(int64(now().Sub(shadowStarted)))
	p.metrics.shadowChecks.Store(checks)
	if err != nil {
		return 0, p.fail(SupportDecisionPublisherStageShadow, err, "verify support decision shadow")
	}
	payload, err := p.encode(table)
	if err != nil || len(payload) == 0 {
		return 0, p.fail(SupportDecisionPublisherStageEncode, err, "encode support decision document")
	}
	if len(payload) > SupportDecisionMaxDocumentSize {
		_ = p.fail(SupportDecisionPublisherStageEncode, errors.New("oversized"), "encode support decision document")
		return 0, fmt.Errorf("encode support decision document: payload exceeds %d bytes", SupportDecisionMaxDocumentSize)
	}
	if _, err := DecodeSupportDecisionDocument(payload, generation); err != nil {
		return 0, p.fail(SupportDecisionPublisherStageVerify, err, "verify encoded support decision document")
	}

	// Ownership can be lost while loading or building; fence every Redis side effect.
	if err := ctx.Err(); err != nil {
		return 0, p.fail(SupportDecisionPublisherStagePut, err, "put support decision document")
	}
	if err := p.store.PutDocument(ctx, generation, payload, p.ttl); err != nil {
		return 0, p.fail(SupportDecisionPublisherStagePut, err, "put support decision document")
	}
	// Leave a successfully written TTL document in place if this ownership term ended.
	if err := ctx.Err(); err != nil {
		return 0, p.fail(SupportDecisionPublisherStageActivate, err, "activate support decision document")
	}
	activated, err := p.store.Activate(ctx, generation)
	if err != nil {
		return 0, p.fail(SupportDecisionPublisherStageActivate, err, "activate support decision document")
	}
	if !activated {
		return 0, p.fail(SupportDecisionPublisherStageActivate, ErrSupportDecisionNotPublished, "activate support decision document")
	}

	var exact, wildcard, hot uint64
	for i := range table.Scopes {
		exact += uint64(len(table.Scopes[i].ExactWire))
		wildcard += uint64(len(table.Scopes[i].Wildcard))
		hot += uint64(len(table.Scopes[i].Hot))
	}
	p.metrics.sequence.Add(1)
	p.metrics.documentBytes.Store(uint64(len(payload)))
	p.metrics.scopeCount.Store(uint64(len(table.Scopes)))
	p.metrics.exactCount.Store(exact)
	p.metrics.wildcardCount.Store(wildcard)
	p.metrics.hotCount.Store(hot)
	p.metrics.lastGeneration.Store(generation)
	completed := now()
	p.metrics.lastSuccessUnixNano.Store(completed.UnixNano())
	p.metrics.lastCompletedUnixNano.Store(completed.UnixNano())
	p.metrics.lastDurationNanos.Store(int64(completed.Sub(started)))
	p.metrics.lastOutcome.Store(1)
	p.metrics.successes.Add(1)
	p.metrics.sequence.Add(1)
	// Pub/Sub only accelerates replica polling and cannot change confirmed success.
	_ = p.store.PublishWakeup(ctx, generation)
	return generation, nil
}

func (p *SupportDecisionPublisher) fail(stage SupportDecisionPublisherStage, err error, operation string) error {
	class := supportDecisionErrorClass(err)
	if stage == SupportDecisionPublisherStageShadow && err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		class = SupportDecisionErrorMismatch
	}
	p.metrics.sequence.Add(1)
	p.metrics.failures[stage][class].Add(1)
	completed := time.Now()
	if p.now != nil {
		completed = p.now()
	}
	p.metrics.lastCompletedUnixNano.Store(completed.UnixNano())
	p.metrics.lastDurationNanos.Store(completed.UnixNano() - p.metrics.attemptStartedUnixNano.Load())
	p.metrics.lastOutcome.Store(2)
	p.metrics.sequence.Add(1)
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if class == SupportDecisionErrorMismatch {
		return fmt.Errorf("%s: stage=shadow class=mismatch: %w", operation, ErrSupportDecisionShadowMismatch)
	}
	if errors.Is(err, ErrSupportDecisionNotPublished) {
		return fmt.Errorf("%s: stage=%s class=%s: %w", operation, stage, class, ErrSupportDecisionNotPublished)
	}
	return fmt.Errorf("%s: stage=%s class=%s", operation, stage, class)
}

func supportDecisionErrorClass(err error) SupportDecisionErrorClass {
	switch {
	case errors.Is(err, context.Canceled):
		return SupportDecisionErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return SupportDecisionErrorDeadline
	default:
		return SupportDecisionErrorOperation
	}
}

func (p *SupportDecisionPublisher) Snapshot() SupportDecisionPublisherSnapshot {
	if p == nil {
		return SupportDecisionPublisherSnapshot{}
	}
	return p.metrics.snapshot()
}
