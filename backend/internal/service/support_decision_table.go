package service

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	SupportDecisionSchemaVersion       uint16 = 1
	SupportDecisionMaxModelBytes              = 256
	SupportDecisionExactLimit                 = 4096
	SupportDecisionWildcardLimit              = 256
	SupportDecisionHotDataLimit               = 4 << 10
	SupportDecisionFallbackDataLimit          = 512 << 10
	SupportDecisionTypicalDocumentSize        = 4 << 20
	SupportDecisionMaxDocumentSize            = 8 << 20

	supportDecisionGenericCoordinateCount = 4
	supportDecisionOpenAICoordinateCount  = 288
)

type supportDecisionScopeKey struct {
	Platform             string
	GroupID              int64
	IncludeGrouped       bool
	AllowMixedScheduling bool
}

type supportDecisionProfile struct {
	SupportBits  []byte `json:"support_bits,omitempty"`
	EligibleBits []byte `json:"eligible_bits,omitempty"`
}

type supportDecisionHotModel struct {
	StringID uint32                 `json:"string_id"`
	Profile  supportDecisionProfile `json:"profile"`
}

type supportDecisionWildcard struct {
	PrefixID uint32                 `json:"prefix_id"`
	TargetID uint32                 `json:"target_id"`
	Profile  supportDecisionProfile `json:"profile"`
}

type supportDecisionScopeTable struct {
	Key                 supportDecisionScopeKey              `json:"key"`
	OpenAI              bool                                 `json:"openai"`
	Hot                 []supportDecisionHotModel            `json:"hot,omitempty"`
	Exact               map[string]supportDecisionExactValue `json:"-"`
	ExactWire           []supportDecisionExactEntry          `json:"exact,omitempty"`
	Wildcard            []supportDecisionWildcard            `json:"wildcard,omitempty"`
	ChannelExact        []uint32                             `json:"channel_exact,omitempty"`
	ChannelWildcard     []uint32                             `json:"channel_wildcard,omitempty"`
	ChannelAllowed      supportDecisionProfile               `json:"channel_allowed,omitempty"`
	KnownCodexSupport   []byte                               `json:"known_codex_support,omitempty"`
	KnownBedrockSupport []byte                               `json:"known_bedrock_support,omitempty"`
	Default             supportDecisionProfile               `json:"default"`
}

type supportDecisionExactEntry struct {
	StringID uint32                 `json:"string_id"`
	TargetID uint32                 `json:"target_id"`
	Profile  supportDecisionProfile `json:"profile"`
}

type supportDecisionExactValue struct {
	TargetID uint32
	Profile  supportDecisionProfile
}

// SupportDecisionTable is immutable after construction or decoding. The source
// snapshot is deliberately not retained.
type SupportDecisionTable struct {
	SchemaVersion uint16                      `json:"schema_version"`
	Generation    uint64                      `json:"generation"`
	Strings       []string                    `json:"strings"`
	Scopes        []supportDecisionScopeTable `json:"scopes"`

	TypicalSizeExceeded bool `json:"-"`
	verified            bool
	scopeIndex          map[supportDecisionScopeKey]int
}

func genericSupportDecisionCoordinate(requiresPrivacy, thinkingEnabled bool) int {
	coordinate := 0
	if requiresPrivacy {
		coordinate |= 1
	}
	if thinkingEnabled {
		coordinate |= 2
	}
	return coordinate
}

func openAISupportDecisionCoordinate(query SupportDecisionQuery) (int, bool) {
	privacy := 0
	if query.RequiresPrivacy {
		privacy = 1
	}
	endpoint, ok := openAIEndpointCoordinate(query.EndpointCapability)
	if !ok {
		return 0, false
	}
	image, ok := openAIImageCoordinate(query.ImageCapability)
	if !ok {
		return 0, false
	}
	compact := 0
	if query.RequireCompact {
		compact = 1
	}
	transport, ok := openAITransportCoordinate(query.Transport)
	if !ok {
		return 0, false
	}
	// ThinkingEnabled is intentionally absent from the OpenAI coordinate.
	return ((((privacy*4)+endpoint)*3+image)*2+compact)*4 + transport, true
}

func openAIEndpointCoordinate(capability OpenAIEndpointCapability) (int, bool) {
	switch capability {
	case "":
		return 0, true
	case OpenAIEndpointCapabilityChatCompletions:
		return 1, true
	case OpenAIEndpointCapabilityEmbeddings:
		return 2, true
	case OpenAIEndpointCapabilityOAuthCompactBodySignal:
		return 3, true
	default:
		return 0, false
	}
}

func openAIImageCoordinate(capability OpenAIImagesCapability) (int, bool) {
	switch capability {
	case "":
		return 0, true
	case OpenAIImagesCapabilityBasic:
		return 1, true
	case OpenAIImagesCapabilityNative:
		return 2, true
	default:
		return 0, false
	}
}

func openAITransportCoordinate(transport OpenAIUpstreamTransport) (int, bool) {
	switch transport {
	case OpenAIUpstreamTransportAny:
		return 0, true
	case OpenAIUpstreamTransportHTTPSSE:
		return 1, true
	case OpenAIUpstreamTransportResponsesWebsocket:
		return 2, true
	case OpenAIUpstreamTransportResponsesWebsocketV2:
		return 3, true
	default:
		return 0, false
	}
}

func profileResult(profile supportDecisionProfile, coordinate int) SupportDecisionResult {
	if bitSet(profile.SupportBits, coordinate) {
		return SupportDecisionNotPureMiss
	}
	if bitSet(profile.EligibleBits, coordinate) {
		return SupportDecisionPureMiss
	}
	return SupportDecisionNotPureMiss
}

func bitSet(bits []byte, coordinate int) bool {
	index := coordinate >> 3
	return index >= 0 && index < len(bits) && bits[index]&(1<<uint(coordinate&7)) != 0
}

func setBit(bits []byte, coordinate int) {
	bits[coordinate>>3] |= 1 << uint(coordinate&7)
}

func (t *SupportDecisionTable) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	if t == nil || !t.verified || t.SchemaVersion != SupportDecisionSchemaVersion {
		return SupportDecisionUnknown
	}
	model := strings.TrimSpace(query.RequestedModel)
	if model == "" || len(model) > SupportDecisionMaxModelBytes || query.Scope.Platform == "" {
		return SupportDecisionUnknown
	}
	index, ok := t.scopeIndex[supportDecisionScopeKey(query.Scope)]
	if !ok {
		return SupportDecisionUnknown
	}
	scope := &t.Scopes[index]
	coordinate := genericSupportDecisionCoordinate(query.RequiresPrivacy, query.ThinkingEnabled)
	if scope.OpenAI {
		var valid bool
		coordinate, valid = openAISupportDecisionCoordinate(query)
		if !valid {
			return SupportDecisionUnknown
		}
	}

	for i := range scope.Hot {
		hot := &scope.Hot[i]
		if int(hot.StringID) < len(t.Strings) && t.Strings[hot.StringID] == model {
			return profileResult(hot.Profile, coordinate)
		}
	}
	if exact, exists := scope.Exact[model]; exists {
		return profileResult(exact.Profile, coordinate)
	}
	// OAuth Codex aliases are an infinite normalized family and therefore remain
	// an explicit fallback fact rather than expanding the exact directory.
	if scope.OpenAI {
		if _, known := normalizeKnownCodexModel(model); known && bitSet(scope.KnownCodexSupport, coordinate) {
			return SupportDecisionNotPureMiss
		}
	} else if _, _, known := normalizeBedrockModelID(model); known && bitSet(scope.KnownBedrockSupport, coordinate) {
		return SupportDecisionNotPureMiss
	}
	for i := range scope.Wildcard {
		rule := &scope.Wildcard[i]
		if int(rule.PrefixID) < len(t.Strings) && strings.HasPrefix(model, t.Strings[rule.PrefixID]) {
			return profileResult(rule.Profile, coordinate)
		}
	}
	if len(scope.ChannelExact) > 0 || len(scope.ChannelWildcard) > 0 {
		if supportDecisionChannelPatternMatches(t.Strings, scope, model) {
			return profileResult(scope.ChannelAllowed, coordinate)
		}
	}
	return profileResult(scope.Default, coordinate)
}

func supportDecisionChannelPatternMatches(stringsTable []string, scope *supportDecisionScopeTable, model string) bool {
	for _, stringID := range scope.ChannelExact {
		if int(stringID) < len(stringsTable) && equalFoldASCII(model, stringsTable[stringID]) {
			return true
		}
	}
	for _, stringID := range scope.ChannelWildcard {
		if int(stringID) < len(stringsTable) && hasPrefixFoldASCII(model, stringsTable[stringID]) {
			return true
		}
	}
	return false
}

func equalFoldASCII(left, right string) bool {
	return len(left) == len(right) && hasPrefixFoldASCII(left, right)
}

func hasPrefixFoldASCII(value, prefix string) bool {
	if len(value) < len(prefix) {
		return false
	}
	for i := range prefix {
		left, right := value[i], prefix[i]
		if left >= 'A' && left <= 'Z' {
			left += 'a' - 'A'
		}
		if right >= 'A' && right <= 'Z' {
			right += 'a' - 'A'
		}
		if left != right {
			return false
		}
	}
	return true
}

func (t *SupportDecisionTable) prepareIndexes() bool {
	if t == nil || t.SchemaVersion != SupportDecisionSchemaVersion || t.Generation == 0 {
		return false
	}
	t.scopeIndex = make(map[supportDecisionScopeKey]int, len(t.Scopes))
	for i := range t.Scopes {
		scope := &t.Scopes[i]
		coordinateCount := supportDecisionGenericCoordinateCount
		if scope.OpenAI {
			if scope.Key.Platform != PlatformOpenAI {
				return false
			}
			coordinateCount = supportDecisionOpenAICoordinateCount
		} else if scope.Key.Platform == PlatformOpenAI {
			return false
		}
		if scope.Key.Platform == "" || len(scope.Hot) > SupportDecisionHotModelLimit || len(scope.ExactWire) > SupportDecisionExactLimit || len(scope.Wildcard) > SupportDecisionWildcardLimit ||
			!validSupportDecisionProfile(scope.Default, coordinateCount) || !validSupportDecisionOptionalBits(scope.KnownCodexSupport, coordinateCount, len(scope.KnownCodexSupport) > 0) ||
			!validSupportDecisionOptionalBits(scope.KnownBedrockSupport, coordinateCount, len(scope.KnownBedrockSupport) > 0) ||
			(len(scope.KnownBedrockSupport) > 0 && scope.Key.Platform != PlatformAnthropic) {
			return false
		}
		if _, exists := t.scopeIndex[scope.Key]; exists {
			return false
		}
		t.scopeIndex[scope.Key] = i
		var previousHot uint32
		for j, hot := range scope.Hot {
			if int(hot.StringID) >= len(t.Strings) || !validSupportDecisionProfile(hot.Profile, coordinateCount) || (j > 0 && previousHot >= hot.StringID) {
				return false
			}
			previousHot = hot.StringID
		}
		scope.Exact = make(map[string]supportDecisionExactValue, len(scope.ExactWire))
		var previousExact uint32
		for j, entry := range scope.ExactWire {
			if int(entry.StringID) >= len(t.Strings) || int(entry.TargetID) >= len(t.Strings) || !validSupportDecisionProfile(entry.Profile, coordinateCount) || (j > 0 && previousExact >= entry.StringID) {
				return false
			}
			previousExact = entry.StringID
			model := t.Strings[entry.StringID]
			if _, exists := scope.Exact[model]; exists {
				return false
			}
			scope.Exact[model] = supportDecisionExactValue{TargetID: entry.TargetID, Profile: entry.Profile}
		}
		seenPrefixes := make(map[uint32]struct{}, len(scope.Wildcard))
		for j := range scope.Wildcard {
			rule := &scope.Wildcard[j]
			if int(rule.PrefixID) >= len(t.Strings) || int(rule.TargetID) >= len(t.Strings) || !validSupportDecisionProfile(rule.Profile, coordinateCount) {
				return false
			}
			if _, exists := seenPrefixes[rule.PrefixID]; exists {
				return false
			}
			seenPrefixes[rule.PrefixID] = struct{}{}
		}
		sort.SliceStable(scope.Wildcard, func(i, j int) bool {
			left, right := t.Strings[scope.Wildcard[i].PrefixID], t.Strings[scope.Wildcard[j].PrefixID]
			if len(left) != len(right) {
				return len(left) > len(right)
			}
			return left < right
		})
	}
	t.verified = true
	return true
}

func validSupportDecisionProfile(profile supportDecisionProfile, coordinateCount int) bool {
	byteCount := (coordinateCount + 7) / 8
	if len(profile.SupportBits) != byteCount || len(profile.EligibleBits) != byteCount {
		return false
	}
	for i := range profile.SupportBits {
		if profile.SupportBits[i]&profile.EligibleBits[i] != 0 {
			return false
		}
	}
	return validSupportDecisionTailBits(profile.SupportBits, coordinateCount) && validSupportDecisionTailBits(profile.EligibleBits, coordinateCount)
}

func validSupportDecisionOptionalBits(bits []byte, coordinateCount int, required bool) bool {
	if !required {
		return len(bits) == 0
	}
	return len(bits) == (coordinateCount+7)/8 && validSupportDecisionTailBits(bits, coordinateCount)
}

func validSupportDecisionTailBits(bits []byte, coordinateCount int) bool {
	if len(bits) == 0 || coordinateCount&7 == 0 {
		return true
	}
	return bits[len(bits)-1]&^byte((1<<uint(coordinateCount&7))-1) == 0
}

// SupportDecisionAtomicReader owns only a verified immutable table pointer.
type SupportDecisionAtomicReader struct {
	table          atomic.Pointer[SupportDecisionTable]
	verifiedUnixNS atomic.Int64
	maxStale       time.Duration
	now            func() time.Time
}

func NewSupportDecisionAtomicReader(maxStale time.Duration) *SupportDecisionAtomicReader {
	return &SupportDecisionAtomicReader{maxStale: maxStale, now: time.Now}
}

func (r *SupportDecisionAtomicReader) Install(table *SupportDecisionTable, verifiedAt time.Time) bool {
	if r == nil || table == nil || !table.verified {
		return false
	}
	r.table.Store(table)
	r.verifiedUnixNS.Store(verifiedAt.UnixNano())
	return true
}

func (r *SupportDecisionAtomicReader) Verify(verifiedAt time.Time) {
	if r != nil && r.table.Load() != nil {
		r.verifiedUnixNS.Store(verifiedAt.UnixNano())
	}
}

func (r *SupportDecisionAtomicReader) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	if r == nil {
		return SupportDecisionUnknown
	}
	table := r.table.Load()
	if table == nil {
		return SupportDecisionUnknown
	}
	if r.maxStale > 0 {
		now := time.Now
		if r.now != nil {
			now = r.now
		}
		if now().Sub(time.Unix(0, r.verifiedUnixNS.Load())) > r.maxStale {
			return SupportDecisionUnknown
		}
	}
	return table.Lookup(query)
}
