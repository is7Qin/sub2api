package service

import (
	"fmt"
	"sync/atomic"
	"time"
)

// SupportDecisionErrorClass is deliberately finite so failures cannot create
// identifier-bearing metric labels or status values.
type SupportDecisionErrorClass uint8

const (
	SupportDecisionErrorOperation SupportDecisionErrorClass = iota
	SupportDecisionErrorCanceled
	SupportDecisionErrorDeadline
	SupportDecisionErrorMismatch
	supportDecisionErrorClassCount
)

func (c SupportDecisionErrorClass) String() string {
	switch c {
	case SupportDecisionErrorCanceled:
		return "canceled"
	case SupportDecisionErrorDeadline:
		return "deadline"
	case SupportDecisionErrorMismatch:
		return "mismatch"
	default:
		return "operation"
	}
}

type SupportDecisionPublisherStage uint8

const (
	SupportDecisionPublisherStageGeneration SupportDecisionPublisherStage = iota
	SupportDecisionPublisherStageSource
	SupportDecisionPublisherStageBuild
	SupportDecisionPublisherStageShadow
	SupportDecisionPublisherStageEncode
	SupportDecisionPublisherStageVerify
	SupportDecisionPublisherStagePut
	SupportDecisionPublisherStageActivate
	supportDecisionPublisherStageCount
)

func (s SupportDecisionPublisherStage) String() string {
	switch s {
	case SupportDecisionPublisherStageGeneration:
		return "generation"
	case SupportDecisionPublisherStageSource:
		return "source"
	case SupportDecisionPublisherStageBuild:
		return "build"
	case SupportDecisionPublisherStageShadow:
		return "shadow"
	case SupportDecisionPublisherStageEncode:
		return "encode"
	case SupportDecisionPublisherStageVerify:
		return "verify"
	case SupportDecisionPublisherStagePut:
		return "put"
	case SupportDecisionPublisherStageActivate:
		return "activate"
	default:
		return "unknown"
	}
}

type supportDecisionPublisherMetrics struct {
	attempts, successes atomic.Uint64
	failures            [supportDecisionPublisherStageCount][supportDecisionErrorClassCount]atomic.Uint64
	active              atomic.Bool
	lastGeneration      atomic.Uint64
	lastSuccessUnixNano atomic.Int64
	buildDurationNanos  atomic.Int64
	documentBytes       atomic.Uint64
	scopeCount          atomic.Uint64
	exactCount          atomic.Uint64
	wildcardCount       atomic.Uint64
	hotCount            atomic.Uint64
	shadowDurationNanos atomic.Int64
	shadowChecks        atomic.Uint64
}

type SupportDecisionPublisherSnapshot struct {
	Attempts                 uint64
	SuccessfulActivations    uint64
	Failures                 [supportDecisionPublisherStageCount][supportDecisionErrorClassCount]uint64
	Active                   bool
	LastSuccessfulGeneration uint64
	LastSuccessfulAt         time.Time
	BuildDuration            time.Duration
	DocumentBytes            uint64
	ScopeCount               uint64
	ExactEntryCount          uint64
	WildcardEntryCount       uint64
	HotEntryCount            uint64
	ShadowDuration           time.Duration
	ShadowChecks             uint64
}

func (s SupportDecisionPublisherSnapshot) FailureCount(stage SupportDecisionPublisherStage, class SupportDecisionErrorClass) uint64 {
	if stage >= supportDecisionPublisherStageCount || class >= supportDecisionErrorClassCount {
		return 0
	}
	return s.Failures[stage][class]
}

func (s SupportDecisionPublisherSnapshot) String() string {
	return fmt.Sprintf("attempts=%d successes=%d active=%t generation=%d bytes=%d scopes=%d exact=%d wildcard=%d hot=%d shadow_checks=%d",
		s.Attempts, s.SuccessfulActivations, s.Active, s.LastSuccessfulGeneration, s.DocumentBytes, s.ScopeCount, s.ExactEntryCount, s.WildcardEntryCount, s.HotEntryCount, s.ShadowChecks)
}

func (m *supportDecisionPublisherMetrics) snapshot() SupportDecisionPublisherSnapshot {
	if m == nil {
		return SupportDecisionPublisherSnapshot{}
	}
	s := SupportDecisionPublisherSnapshot{
		Attempts: m.attempts.Load(), SuccessfulActivations: m.successes.Load(), Active: m.active.Load(),
		LastSuccessfulGeneration: m.lastGeneration.Load(), BuildDuration: time.Duration(m.buildDurationNanos.Load()),
		DocumentBytes: m.documentBytes.Load(), ScopeCount: m.scopeCount.Load(), ExactEntryCount: m.exactCount.Load(),
		WildcardEntryCount: m.wildcardCount.Load(), HotEntryCount: m.hotCount.Load(),
		ShadowDuration: time.Duration(m.shadowDurationNanos.Load()), ShadowChecks: m.shadowChecks.Load(),
	}
	if unixNano := m.lastSuccessUnixNano.Load(); unixNano != 0 {
		s.LastSuccessfulAt = time.Unix(0, unixNano)
	}
	for stage := range s.Failures {
		for class := range s.Failures[stage] {
			s.Failures[stage][class] = m.failures[stage][class].Load()
		}
	}
	return s
}

type SupportDecisionReplicaStage uint8

const (
	SupportDecisionReplicaStageActive SupportDecisionReplicaStage = iota
	SupportDecisionReplicaStageFetch
	SupportDecisionReplicaStageDecode
	SupportDecisionReplicaStageInstall
	supportDecisionReplicaStageCount
)

type supportDecisionReplicaMetrics struct {
	polls, wakeups, installs, verifications atomic.Uint64
	failures                                [supportDecisionReplicaStageCount][supportDecisionErrorClassCount]atomic.Uint64
	activeGeneration                        atomic.Uint64
	documentBytes                           atomic.Uint64
	decodeDurationNanos                     atomic.Int64
	installDurationNanos                    atomic.Int64
}

type SupportDecisionReplicaSnapshot struct {
	Polls, Wakeups, SuccessfulInstalls, VerificationRefreshes uint64
	Failures                                                  [supportDecisionReplicaStageCount][supportDecisionErrorClassCount]uint64
	ActiveGeneration, InstalledGeneration, DocumentBytes      uint64
	ActiveGenerationFailures                                  uint64
	LastVerifiedAt                                            time.Time
	VerificationAge                                           time.Duration
	Stale, Unknown                                            bool
	DecodeDuration, InstallDuration                           time.Duration
}

func (s SupportDecisionReplicaSnapshot) FailureCount(stage SupportDecisionReplicaStage, class SupportDecisionErrorClass) uint64 {
	if stage >= supportDecisionReplicaStageCount || class >= supportDecisionErrorClassCount {
		return 0
	}
	return s.Failures[stage][class]
}
func (s SupportDecisionReplicaSnapshot) String() string {
	return fmt.Sprintf("polls=%d wakeups=%d installs=%d verifications=%d active_generation=%d installed_generation=%d bytes=%d stale=%t unknown=%t",
		s.Polls, s.Wakeups, s.SuccessfulInstalls, s.VerificationRefreshes, s.ActiveGeneration, s.InstalledGeneration, s.DocumentBytes, s.Stale, s.Unknown)
}

type SupportDecisionLookupSnapshot struct {
	TotalLookups, UnknownLookups, NotPureMissLookups, PureMissLookups uint64
	Generation, DocumentBytes                                         uint64
	LastVerifiedAt, SnapshotAt                                        time.Time
	VerificationAge                                                   time.Duration
	Stale, Unknown                                                    bool
}
