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

type supportDecisionCatchAll struct {
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
	CatchAll            *supportDecisionCatchAll             `json:"catch_all,omitempty"`
	ChannelExact        []uint32                             `json:"channel_exact,omitempty"`
	ChannelWildcard     []uint32                             `json:"channel_wildcard,omitempty"`
	ChannelCatchAll     bool                                 `json:"channel_catch_all,omitempty"`
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
	wirePayload         []byte
	schemaVersion       uint16
	generation          uint64
	runtimeStrings      []string
	runtimeScopes       []supportDecisionScopeTable
	scopeIndex          map[supportDecisionScopeKey]int
	shadowScopes        []supportDecisionTemporaryScope
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
	if t == nil || !t.verified || t.schemaVersion != SupportDecisionSchemaVersion {
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
	scope := &t.runtimeScopes[index]
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
		if int(hot.StringID) < len(t.runtimeStrings) && t.runtimeStrings[hot.StringID] == model {
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
		if int(rule.PrefixID) < len(t.runtimeStrings) && strings.HasPrefix(model, t.runtimeStrings[rule.PrefixID]) {
			return profileResult(rule.Profile, coordinate)
		}
	}
	if scope.CatchAll != nil {
		return profileResult(scope.CatchAll.Profile, coordinate)
	}
	if scope.ChannelCatchAll || len(scope.ChannelExact) > 0 || len(scope.ChannelWildcard) > 0 {
		if supportDecisionChannelPatternMatches(t.runtimeStrings, scope, model) {
			return profileResult(scope.ChannelAllowed, coordinate)
		}
	}
	return profileResult(scope.Default, coordinate)
}

func supportDecisionChannelPatternMatches(stringsTable []string, scope *supportDecisionScopeTable, model string) bool {
	if scope.ChannelCatchAll {
		return true
	}
	alias := claudePricingRevisionAliasFoldASCII(model)
	for _, stringID := range scope.ChannelExact {
		if int(stringID) < len(stringsTable) && (equalFoldASCII(model, stringsTable[stringID]) || aliasMatchesFoldASCII(model, alias, stringsTable[stringID], false)) {
			return true
		}
	}
	for _, stringID := range scope.ChannelWildcard {
		if int(stringID) < len(stringsTable) && (hasPrefixFoldASCII(model, stringsTable[stringID]) || aliasMatchesFoldASCII(model, alias, stringsTable[stringID], true)) {
			return true
		}
	}
	return false
}

func claudePricingRevisionAliasFoldASCII(model string) int {
	if !hasPrefixFoldASCII(model, "claude-") {
		return -1
	}
	separator := -1
	for i := len(model) - 1; i >= 0; i-- {
		if model[i] == '.' || model[i] == '-' {
			separator = i
			break
		}
	}
	if separator <= len("claude-") || separator == len(model)-1 || model[separator-1] < '0' || model[separator-1] > '9' {
		return -1
	}
	minor := model[separator+1:]
	if len(minor) > 2 {
		return -1
	}
	for i := range minor {
		if minor[i] < '0' || minor[i] > '9' {
			return -1
		}
	}
	return separator
}

func aliasMatchesFoldASCII(model string, separator int, configured string, prefix bool) bool {
	if separator < 0 {
		return false
	}
	aliasLength := len(model)
	if prefix {
		if len(configured) > aliasLength {
			return false
		}
	} else if len(configured) != aliasLength {
		return false
	}
	for i := range configured {
		actual := model[i]
		if i == separator {
			if actual == '.' {
				actual = '-'
			} else {
				actual = '.'
			}
		}
		expected := configured[i]
		if actual >= 'A' && actual <= 'Z' {
			actual += 'a' - 'A'
		}
		if expected >= 'A' && expected <= 'Z' {
			expected += 'a' - 'A'
		}
		if actual != expected {
			return false
		}
	}
	return true
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
	t.schemaVersion = t.SchemaVersion
	t.generation = t.Generation
	t.runtimeStrings = append([]string(nil), t.Strings...)
	t.runtimeScopes = cloneSupportDecisionScopes(t.Scopes)
	t.scopeIndex = make(map[supportDecisionScopeKey]int, len(t.runtimeScopes))
	for i := range t.runtimeScopes {
		scope := &t.runtimeScopes[i]
		coordinateCount := supportDecisionGenericCoordinateCount
		if scope.OpenAI {
			if scope.Key.Platform != PlatformOpenAI {
				return false
			}
			coordinateCount = supportDecisionOpenAICoordinateCount
		} else if scope.Key.Platform == PlatformOpenAI {
			return false
		}
		if scope.Key.Platform == "" || len(scope.Hot) > SupportDecisionHotModelLimit ||
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
			if int(hot.StringID) >= len(t.runtimeStrings) || !validSupportDecisionProfile(hot.Profile, coordinateCount) || (j > 0 && previousHot >= hot.StringID) {
				return false
			}
			previousHot = hot.StringID
		}
		scope.Exact = make(map[string]supportDecisionExactValue, len(scope.ExactWire))
		var previousExact uint32
		for j, entry := range scope.ExactWire {
			if int(entry.StringID) >= len(t.runtimeStrings) || int(entry.TargetID) >= len(t.runtimeStrings) || !validSupportDecisionProfile(entry.Profile, coordinateCount) || (j > 0 && previousExact >= entry.StringID) {
				return false
			}
			previousExact = entry.StringID
			model := t.runtimeStrings[entry.StringID]
			if _, exists := scope.Exact[model]; exists {
				return false
			}
			scope.Exact[model] = supportDecisionExactValue{TargetID: entry.TargetID, Profile: entry.Profile}
		}
		seenPrefixes := make(map[uint32]struct{}, len(scope.Wildcard))
		for j := range scope.Wildcard {
			rule := &scope.Wildcard[j]
			if int(rule.PrefixID) >= len(t.runtimeStrings) || int(rule.TargetID) >= len(t.runtimeStrings) || !validSupportDecisionProfile(rule.Profile, coordinateCount) {
				return false
			}
			if _, exists := seenPrefixes[rule.PrefixID]; exists {
				return false
			}
			seenPrefixes[rule.PrefixID] = struct{}{}
		}
		if scope.CatchAll != nil && (int(scope.CatchAll.TargetID) >= len(t.runtimeStrings) || !validSupportDecisionProfile(scope.CatchAll.Profile, coordinateCount)) {
			return false
		}
		if !validSupportDecisionChannelFacts(t.runtimeStrings, scope, coordinateCount) {
			return false
		}
		if !sort.SliceIsSorted(scope.Wildcard, func(i, j int) bool {
			left, right := t.runtimeStrings[scope.Wildcard[i].PrefixID], t.runtimeStrings[scope.Wildcard[j].PrefixID]
			if len(left) != len(right) {
				return len(left) > len(right)
			}
			return left < right
		}) {
			return false
		}
	}
	t.verified = true
	return true
}

func cloneSupportDecisionScopes(scopes []supportDecisionScopeTable) []supportDecisionScopeTable {
	cloned := make([]supportDecisionScopeTable, len(scopes))
	for i := range scopes {
		cloned[i] = scopes[i]
		cloned[i].Hot = append([]supportDecisionHotModel(nil), scopes[i].Hot...)
		cloned[i].ExactWire = append([]supportDecisionExactEntry(nil), scopes[i].ExactWire...)
		cloned[i].Wildcard = append([]supportDecisionWildcard(nil), scopes[i].Wildcard...)
		cloned[i].ChannelExact = append([]uint32(nil), scopes[i].ChannelExact...)
		cloned[i].ChannelWildcard = append([]uint32(nil), scopes[i].ChannelWildcard...)
		cloned[i].KnownCodexSupport = append([]byte(nil), scopes[i].KnownCodexSupport...)
		cloned[i].KnownBedrockSupport = append([]byte(nil), scopes[i].KnownBedrockSupport...)
		cloned[i].ChannelAllowed = cloneSupportDecisionProfile(scopes[i].ChannelAllowed)
		cloned[i].Default = cloneSupportDecisionProfile(scopes[i].Default)
		if scopes[i].CatchAll != nil {
			catchAll := *scopes[i].CatchAll
			catchAll.Profile = cloneSupportDecisionProfile(catchAll.Profile)
			cloned[i].CatchAll = &catchAll
		}
		for j := range cloned[i].Hot {
			cloned[i].Hot[j].Profile = cloneSupportDecisionProfile(cloned[i].Hot[j].Profile)
		}
		for j := range cloned[i].ExactWire {
			cloned[i].ExactWire[j].Profile = cloneSupportDecisionProfile(cloned[i].ExactWire[j].Profile)
		}
		for j := range cloned[i].Wildcard {
			cloned[i].Wildcard[j].Profile = cloneSupportDecisionProfile(cloned[i].Wildcard[j].Profile)
		}
	}
	return cloned
}

func cloneSupportDecisionProfile(profile supportDecisionProfile) supportDecisionProfile {
	return supportDecisionProfile{
		SupportBits:  append([]byte(nil), profile.SupportBits...),
		EligibleBits: append([]byte(nil), profile.EligibleBits...),
	}
}

func validSupportDecisionChannelFacts(stringsTable []string, scope *supportDecisionScopeTable, coordinateCount int) bool {
	hasPatterns := scope.ChannelCatchAll || len(scope.ChannelExact) > 0 || len(scope.ChannelWildcard) > 0
	if !hasPatterns {
		return len(scope.ChannelAllowed.SupportBits) == 0 && len(scope.ChannelAllowed.EligibleBits) == 0
	}
	if !validSupportDecisionProfile(scope.ChannelAllowed, coordinateCount) {
		return false
	}
	seen := make(map[uint32]struct{}, len(scope.ChannelExact)+len(scope.ChannelWildcard))
	for _, ids := range [][]uint32{scope.ChannelExact, scope.ChannelWildcard} {
		previous := ""
		for _, id := range ids {
			if int(id) >= len(stringsTable) {
				return false
			}
			value := stringsTable[id]
			if previous != "" && previous >= value {
				return false
			}
			if _, exists := seen[id]; exists {
				return false
			}
			seen[id] = struct{}{}
			previous = value
		}
	}
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

type supportDecisionReaderState struct {
	table      *SupportDecisionTable
	verifiedAt time.Time
}

// SupportDecisionAtomicReader owns one atomic snapshot containing the verified
// immutable table and its verification time.
type SupportDecisionAtomicReader struct {
	state    atomic.Pointer[supportDecisionReaderState]
	maxStale time.Duration
	now      func() time.Time
	lookups  atomic.Uint64
	unknown  atomic.Uint64
	notPure  atomic.Uint64
	pure     atomic.Uint64
}

func NewSupportDecisionAtomicReader(maxStale time.Duration) *SupportDecisionAtomicReader {
	return &SupportDecisionAtomicReader{maxStale: maxStale, now: time.Now}
}

func (r *SupportDecisionAtomicReader) Install(table *SupportDecisionTable, verifiedAt time.Time) bool {
	if r == nil || table == nil || !table.verified {
		return false
	}
	frozen, ok := table.frozenCopy()
	if !ok {
		return false
	}
	r.state.Store(&supportDecisionReaderState{table: frozen, verifiedAt: verifiedAt})
	return true
}

func (r *SupportDecisionAtomicReader) installNewer(table *SupportDecisionTable, verifiedAt time.Time) bool {
	if r == nil || table == nil || !table.verified {
		return false
	}
	for {
		current := r.state.Load()
		if current != nil && current.table.generation >= table.generation {
			return false
		}
		next := &supportDecisionReaderState{table: table, verifiedAt: verifiedAt}
		if r.state.CompareAndSwap(current, next) {
			return true
		}
	}
}

func (t *SupportDecisionTable) frozenCopy() (*SupportDecisionTable, bool) {
	payload, err := EncodeSupportDecisionDocument(t)
	if err != nil {
		return nil, false
	}
	frozen, err := DecodeSupportDecisionDocument(payload, t.generation)
	if err != nil {
		return nil, false
	}
	return frozen, true
}

func (r *SupportDecisionAtomicReader) Verify(verifiedAt time.Time) {
	if r == nil {
		return
	}
	for {
		current := r.state.Load()
		if current == nil {
			return
		}
		next := &supportDecisionReaderState{table: current.table, verifiedAt: verifiedAt}
		if r.state.CompareAndSwap(current, next) {
			return
		}
	}
}

func (r *SupportDecisionAtomicReader) verifyGeneration(generation uint64, verifiedAt time.Time) bool {
	if r == nil {
		return false
	}
	for {
		current := r.state.Load()
		if current == nil || current.table.generation != generation {
			return false
		}
		next := &supportDecisionReaderState{table: current.table, verifiedAt: verifiedAt}
		if r.state.CompareAndSwap(current, next) {
			return true
		}
	}
}

func (r *SupportDecisionAtomicReader) generation() uint64 {
	if r == nil {
		return 0
	}
	state := r.state.Load()
	if state == nil {
		return 0
	}
	return state.table.generation
}

func (r *SupportDecisionAtomicReader) verifiedAt() time.Time {
	if r == nil {
		return time.Time{}
	}
	state := r.state.Load()
	if state == nil {
		return time.Time{}
	}
	return state.verifiedAt
}

func (r *SupportDecisionAtomicReader) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	if r == nil {
		return SupportDecisionUnknown
	}
	r.lookups.Add(1)
	state := r.state.Load()
	if state == nil {
		r.unknown.Add(1)
		return SupportDecisionUnknown
	}
	if r.maxStale > 0 {
		now := time.Now
		if r.now != nil {
			now = r.now
		}
		if now().Sub(state.verifiedAt) > r.maxStale {
			r.unknown.Add(1)
			return SupportDecisionUnknown
		}
	}
	result := state.table.Lookup(query)
	switch result {
	case SupportDecisionPureMiss:
		r.pure.Add(1)
	case SupportDecisionNotPureMiss:
		r.notPure.Add(1)
	default:
		r.unknown.Add(1)
	}
	return result
}

func (r *SupportDecisionAtomicReader) Snapshot() SupportDecisionLookupSnapshot {
	if r == nil {
		return SupportDecisionLookupSnapshot{Unknown: true}
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	snapshotAt := now()
	s := SupportDecisionLookupSnapshot{TotalLookups: r.lookups.Load(), UnknownLookups: r.unknown.Load(), NotPureMissLookups: r.notPure.Load(), PureMissLookups: r.pure.Load(), SnapshotAt: snapshotAt}
	state := r.state.Load()
	if state == nil {
		s.Unknown = true
		return s
	}
	s.Generation, s.LastVerifiedAt = state.table.generation, state.verifiedAt
	s.DocumentBytes = uint64(len(state.table.wirePayload))
	s.VerificationAge = snapshotAt.Sub(state.verifiedAt)
	s.Stale = r.maxStale > 0 && s.VerificationAge > r.maxStale
	s.Unknown = s.Stale
	return s
}
