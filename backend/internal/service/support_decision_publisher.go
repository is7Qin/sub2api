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
	}
}

// Publish builds and attempts to activate exactly one newly allocated
// generation. A nonzero generation is returned only after activation is
// confirmed; wakeup delivery remains best effort.
func (p *SupportDecisionPublisher) Publish(ctx context.Context) (uint64, error) {
	if p == nil || p.generation == nil || p.source == nil || p.store == nil || (p.build == nil && p.buildContext == nil) || p.encode == nil {
		return 0, errors.New("support decision publisher dependencies are incomplete")
	}
	if ctx == nil {
		return 0, errors.New("support decision publisher context is nil")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	generation, err := p.generation.NextSupportDecisionGeneration(ctx)
	if err != nil {
		return 0, fmt.Errorf("allocate support decision generation: %w", err)
	}
	if generation == 0 {
		return 0, errors.New("allocate support decision generation: invalid zero generation")
	}

	snapshot, err := p.source.Load(ctx)
	if err != nil {
		return 0, fmt.Errorf("load support decision source: %w", err)
	}
	options := p.options
	options.Generation = generation
	var table *SupportDecisionTable
	if p.buildContext != nil && (p.build == nil || reflect.ValueOf(p.build).Pointer() == reflect.ValueOf(BuildSupportDecisionTable).Pointer()) {
		table, err = p.buildContext(ctx, snapshot, options)
	} else {
		table, err = p.build(snapshot, options)
	}
	if err != nil {
		return 0, fmt.Errorf("build support decision document: %w", err)
	}
	if table == nil || !table.verified || table.Generation != generation {
		return 0, errors.New("build support decision document: invalid verified generation")
	}
	payload, err := p.encode(table)
	if err != nil {
		return 0, fmt.Errorf("encode support decision document: %w", err)
	}
	if len(payload) == 0 {
		return 0, errors.New("encode support decision document: empty payload")
	}
	if len(payload) > SupportDecisionMaxDocumentSize {
		return 0, fmt.Errorf("encode support decision document: payload exceeds %d bytes", SupportDecisionMaxDocumentSize)
	}
	if _, err := DecodeSupportDecisionDocument(payload, generation); err != nil {
		return 0, fmt.Errorf("verify encoded support decision document: %w", err)
	}

	// Ownership can be lost while loading or building; fence every Redis side effect.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := p.store.PutDocument(ctx, generation, payload, p.ttl); err != nil {
		return 0, fmt.Errorf("put support decision document: %w", err)
	}
	// Leave a successfully written TTL document in place if this ownership term ended.
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	activated, err := p.store.Activate(ctx, generation)
	if err != nil {
		return 0, fmt.Errorf("activate support decision document: %w", err)
	}
	if !activated {
		return 0, fmt.Errorf("activate support decision generation %d: %w", generation, ErrSupportDecisionNotPublished)
	}

	// Pub/Sub only accelerates replica polling and cannot change confirmed success.
	_ = p.store.PublishWakeup(ctx, generation)
	return generation, nil
}
