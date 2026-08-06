//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type fixedSupportDecisionReader SupportDecisionResult

func (r fixedSupportDecisionReader) Lookup(SupportDecisionQuery) SupportDecisionResult {
	return SupportDecisionResult(r)
}

type recordingSupportDecisionReader struct {
	result  SupportDecisionResult
	queries []SupportDecisionQuery
}

func (r *recordingSupportDecisionReader) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	r.queries = append(r.queries, query)
	return r.result
}

func TestGatewayPureModelSupportMissUsesLocalDecision(t *testing.T) {
	groupID := int64(42)
	tests := []struct {
		name       string
		ctx        context.Context
		model      string
		platform   string
		excluded   map[int64]struct{}
		mixed      bool
		group      *Group
		groupID    *int64
		cfg        *config.Config
		reader     *recordingSupportDecisionReader
		want       bool
		wantLookup int
		wantQuery  SupportDecisionQuery
	}{
		{
			name:       "grouped mixed private thinking pure miss",
			ctx:        WithThinkingEnabled(WithPublicModelSupportMiss404(context.Background()), true, false),
			model:      " claude-sonnet-4-5 ",
			platform:   PlatformAnthropic,
			mixed:      true,
			group:      &Group{ID: groupID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true, RequirePrivacySet: true},
			groupID:    &groupID,
			cfg:        testConfig(),
			reader:     &recordingSupportDecisionReader{result: SupportDecisionPureMiss},
			want:       true,
			wantLookup: 1,
			wantQuery: SupportDecisionQuery{
				Scope:          SupportDecisionScope{Platform: PlatformAnthropic, GroupID: groupID, AllowMixedScheduling: true},
				RequestedModel: "claude-sonnet-4-5", RequiresPrivacy: true, ThinkingEnabled: true,
			},
		},
		{
			name:       "simple default includes grouped",
			ctx:        WithPublicModelSupportMiss404(context.Background()),
			model:      "gemini-2.5-pro",
			platform:   PlatformGemini,
			mixed:      true,
			cfg:        &config.Config{RunMode: config.RunModeSimple},
			reader:     &recordingSupportDecisionReader{result: SupportDecisionPureMiss},
			want:       true,
			wantLookup: 1,
			wantQuery: SupportDecisionQuery{
				Scope:          SupportDecisionScope{Platform: PlatformGemini, IncludeGrouped: true, AllowMixedScheduling: true},
				RequestedModel: "gemini-2.5-pro",
			},
		},
		{
			name:       "default non-simple excludes grouped",
			ctx:        WithPublicModelSupportMiss404(context.Background()),
			model:      "claude-opus-4-1",
			platform:   PlatformAnthropic,
			cfg:        testConfig(),
			reader:     &recordingSupportDecisionReader{result: SupportDecisionNotPureMiss},
			wantLookup: 1,
			wantQuery:  SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic}, RequestedModel: "claude-opus-4-1"},
		},
		{name: "unknown fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionUnknown}, wantLookup: 1, wantQuery: SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformAnthropic}, RequestedModel: "model"}},
		{name: "nil reader fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, cfg: testConfig()},
		{name: "nil service fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "blank model skips lookup", ctx: WithPublicModelSupportMiss404(context.Background()), model: "  ", platform: PlatformAnthropic, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "disabled skips lookup", ctx: context.Background(), model: "model", platform: PlatformAnthropic, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "exclusion skips lookup", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, excluded: map[int64]struct{}{1: {}}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "unresolved config fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "unresolved platform fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope without group fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, groupID: &groupID, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope with mismatched group fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, groupID: &groupID, group: &Group{ID: groupID + 1, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope with mismatched platform fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, groupID: &groupID, group: &Group{ID: groupID, Platform: PlatformGemini, Status: StatusActive, Hydrated: true}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope with unresolved group fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "model", platform: PlatformAnthropic, groupID: &groupID, group: &Group{ID: groupID, Platform: PlatformAnthropic}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var svc *GatewayService
			if tt.name != "nil service fails closed" {
				svc = &GatewayService{cfg: tt.cfg}
				if tt.reader != nil {
					svc.supportDecisionReader = tt.reader
				}
			}
			accounts := make([]Account, 30000)
			got := svc.isPureModelSupportMiss(tt.ctx, accounts, tt.model, tt.platform, tt.excluded, tt.mixed, tt.group, tt.groupID)
			require.Equal(t, tt.want, got)
			if tt.reader != nil {
				require.Len(t, tt.reader.queries, tt.wantLookup)
				if tt.wantLookup == 1 {
					require.Equal(t, tt.wantQuery, tt.reader.queries[0])
				}
			}
		})
	}
}

func TestOpenAIPureModelSupportMissUsesLocalDecision(t *testing.T) {
	groupID := int64(7)
	tests := []struct {
		name       string
		ctx        context.Context
		model      string
		excluded   map[int64]struct{}
		groupID    *int64
		group      *Group
		cfg        *config.Config
		reader     *recordingSupportDecisionReader
		compact    bool
		endpoint   OpenAIEndpointCapability
		image      OpenAIImagesCapability
		transport  OpenAIUpstreamTransport
		want       bool
		wantLookup int
		wantQuery  SupportDecisionQuery
	}{
		{
			name: "grouped capability coordinates", ctx: WithPublicModelSupportMiss404(context.Background()), model: " gpt-external ", groupID: &groupID,
			group: &Group{ID: groupID, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true, RequirePrivacySet: true}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss},
			compact: true, endpoint: OpenAIEndpointCapabilityOAuthCompactBodySignal, image: OpenAIImagesCapabilityNative, transport: OpenAIUpstreamTransportResponsesWebsocketV2,
			want: true, wantLookup: 1,
			wantQuery: SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, GroupID: groupID}, RequestedModel: "gpt-external", RequiresPrivacy: true, EndpointCapability: OpenAIEndpointCapabilityOAuthCompactBodySignal, ImageCapability: OpenAIImagesCapabilityNative, RequireCompact: true, Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
		},
		{
			name: "simple default scope", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", cfg: &config.Config{RunMode: config.RunModeSimple}, reader: &recordingSupportDecisionReader{result: SupportDecisionNotPureMiss}, wantLookup: 1,
			wantQuery: SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI, IncludeGrouped: true}, RequestedModel: "gpt-5", Transport: OpenAIUpstreamTransportAny},
		},
		{name: "unknown fails closed", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionUnknown}, transport: OpenAIUpstreamTransportHTTPSSE, wantLookup: 1, wantQuery: SupportDecisionQuery{Scope: SupportDecisionScope{Platform: PlatformOpenAI}, RequestedModel: "gpt-5", Transport: OpenAIUpstreamTransportHTTPSSE}},
		{name: "nil reader", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", cfg: testConfig()},
		{name: "nil service", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "blank model", ctx: WithPublicModelSupportMiss404(context.Background()), model: " ", cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "disabled", ctx: context.Background(), model: "gpt-5", cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "excluded", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", excluded: map[int64]struct{}{9: {}}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "unresolved config", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope without group", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", groupID: &groupID, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope with mismatched group", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", groupID: &groupID, group: &Group{ID: groupID + 1, Platform: PlatformOpenAI, Status: StatusActive, Hydrated: true}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope with mismatched platform", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", groupID: &groupID, group: &Group{ID: groupID, Platform: PlatformAnthropic, Status: StatusActive, Hydrated: true}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
		{name: "grouped scope with unresolved group", ctx: WithPublicModelSupportMiss404(context.Background()), model: "gpt-5", groupID: &groupID, group: &Group{ID: groupID, Platform: PlatformOpenAI}, cfg: testConfig(), reader: &recordingSupportDecisionReader{result: SupportDecisionPureMiss}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var svc *OpenAIGatewayService
			if tt.name != "nil service" {
				svc = &OpenAIGatewayService{cfg: tt.cfg}
				if tt.reader != nil {
					svc.supportDecisionReader = tt.reader
				}
			}
			accounts := make([]Account, 30000)
			got := isPureOpenAIModelSupportMiss(tt.ctx, svc, tt.groupID, accounts, tt.model, tt.excluded, tt.compact, tt.endpoint, tt.image, tt.transport, tt.group)
			require.Equal(t, tt.want, got)
			if tt.reader != nil {
				require.Len(t, tt.reader.queries, tt.wantLookup)
				if tt.wantLookup == 1 {
					require.Equal(t, tt.wantQuery, tt.reader.queries[0])
				}
			}
		})
	}
}

func TestPureModelSupportMissDoesNotUseRepositoriesOrScanAccounts(t *testing.T) {
	panicRepo := &mockAccountRepoForPlatform{
		listModelAvailabilityCandidates: func(context.Context, *int64, []string, bool) ([]Account, error) {
			panic("classifier must not query repositories")
		},
	}
	ctx := WithPublicModelSupportMiss404(context.Background())
	for _, count := range []int{100, 10000, 30000} {
		genericReader := &recordingSupportDecisionReader{result: SupportDecisionPureMiss}
		generic := &GatewayService{accountRepo: panicRepo, cfg: testConfig(), supportDecisionReader: genericReader}
		require.True(t, generic.isPureModelSupportMiss(ctx, make([]Account, count), "model", PlatformAnthropic, nil, false, nil, nil))
		require.Len(t, genericReader.queries, 1)

		openAIReader := &recordingSupportDecisionReader{result: SupportDecisionPureMiss}
		openAI := &OpenAIGatewayService{accountRepo: panicRepo, cfg: testConfig(), supportDecisionReader: openAIReader}
		require.True(t, isPureOpenAIModelSupportMiss(ctx, openAI, nil, make([]Account, count), "model", nil, false, "", "", OpenAIUpstreamTransportAny, nil))
		require.Len(t, openAIReader.queries, 1)
	}
}
