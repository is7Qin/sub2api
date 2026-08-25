//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

type channelMonitorV2AggregatorRepoStub struct {
	config     ChannelMonitorV2Config
	watermark  ChannelMonitorV2AggregationWatermark
	configGets int
	ranges     [][2]time.Time
}

func (s *channelMonitorV2AggregatorRepoStub) GetConfig(context.Context) (*ChannelMonitorV2Config, error) {
	s.configGets++
	cfg := s.config
	return &cfg, nil
}
func (*channelMonitorV2AggregatorRepoStub) UpdateConfig(context.Context, ChannelMonitorV2Config, int) (*ChannelMonitorV2Config, error) {
	return nil, nil
}
func (*channelMonitorV2AggregatorRepoStub) GetDimensions(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config) (*ChannelMonitorV2Dimensions, error) {
	return nil, nil
}
func (*channelMonitorV2AggregatorRepoStub) GetSnapshot(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config, bool) (*ChannelMonitorV2Snapshot, error) {
	return nil, nil
}
func (*channelMonitorV2AggregatorRepoStub) GetModels(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config, bool) (*ChannelMonitorV2List[ChannelMonitorV2ModelRow], error) {
	return nil, nil
}
func (*channelMonitorV2AggregatorRepoStub) GetMatrix(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config, ChannelMonitorV2GroupBy, bool) (*ChannelMonitorV2Matrix, error) {
	return nil, nil
}
func (*channelMonitorV2AggregatorRepoStub) GetErrors(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config, bool) (*ChannelMonitorV2List[ChannelMonitorV2ErrorRow], error) {
	return nil, nil
}
func (*channelMonitorV2AggregatorRepoStub) GetUsers(context.Context, ChannelMonitorV2Filter, ChannelMonitorV2Config, bool) (*ChannelMonitorV2List[ChannelMonitorV2UserRow], error) {
	return nil, nil
}
func (s *channelMonitorV2AggregatorRepoStub) GetAggregationWatermark(context.Context) (*ChannelMonitorV2AggregationWatermark, error) {
	wm := s.watermark
	return &wm, nil
}
func (s *channelMonitorV2AggregatorRepoStub) RecomputeRange(_ context.Context, start, end time.Time) error {
	s.ranges = append(s.ranges, [2]time.Time{start, end})
	return nil
}

type channelMonitorRuntimeReaderStub struct {
	runtime ChannelMonitorRuntime
}

func (s channelMonitorRuntimeReaderStub) GetChannelMonitorRuntime(context.Context) ChannelMonitorRuntime {
	return s.runtime
}

func TestChannelMonitorRuntimeKeepsV1AndV2MutuallyExclusive(t *testing.T) {
	require.True(t, (ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV1}).ActiveProbesAllowed())
	require.False(t, (ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV1}).PassiveAggregationAllowed())
	require.False(t, (ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV2}).ActiveProbesAllowed())
	require.True(t, (ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV2}).PassiveAggregationAllowed())
	require.False(t, (ChannelMonitorRuntime{Enabled: false, Mode: ChannelMonitorModeV2}).PassiveAggregationAllowed())
}

func TestGetChannelMonitorRuntimeDefaultsToSafeV1AndHiddenThroughput(t *testing.T) {
	runtime := NewSettingService(&settingRepoStub{values: map[string]string{}}, nil).GetChannelMonitorRuntime(context.Background())
	require.True(t, runtime.ActiveProbesAllowed())
	require.False(t, runtime.PassiveAggregationAllowed())
	require.True(t, runtime.HideThroughput)

	runtime = NewSettingService(&settingRepoStub{err: context.DeadlineExceeded}, nil).GetChannelMonitorRuntime(context.Background())
	require.True(t, runtime.ActiveProbesAllowed())
	require.False(t, runtime.PassiveAggregationAllowed())
	require.True(t, runtime.HideThroughput)
}

func TestGetChannelMonitorRuntimeEnablesV2OnlyExplicitly(t *testing.T) {
	runtime := NewSettingService(&settingRepoStub{values: map[string]string{
		SettingKeyChannelMonitorEnabled:        "true",
		SettingKeyChannelMonitorMode:           ChannelMonitorModeV2,
		SettingKeyChannelMonitorHideThroughput: "false",
	}}, nil).GetChannelMonitorRuntime(context.Background())
	require.False(t, runtime.ActiveProbesAllowed())
	require.True(t, runtime.PassiveAggregationAllowed())
	require.False(t, runtime.HideThroughput)
}

func TestChannelMonitorV2AggregatorFailsClosedOutsideEnabledV2Mode(t *testing.T) {
	for _, runtime := range []ChannelMonitorRuntime{
		{},
		{Enabled: true, Mode: ChannelMonitorModeV1},
		{Enabled: false, Mode: ChannelMonitorModeV2},
	} {
		repo := &channelMonitorV2AggregatorRepoStub{config: ChannelMonitorV2Config{Enabled: true, RefreshIntervalSeconds: 60}}
		agg := NewChannelMonitorV2Aggregator(repo, channelMonitorRuntimeReaderStub{runtime: runtime}, nil, nil)
		require.NoError(t, agg.Run(context.Background()))
		require.Zero(t, repo.configGets)
		require.Empty(t, repo.ranges)
	}
}

func TestChannelMonitorV2AggregatorSeedsRecentWindowThenHonorsRefreshInterval(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	repo := &channelMonitorV2AggregatorRepoStub{config: ChannelMonitorV2Config{Enabled: true, RefreshIntervalSeconds: 300}}
	agg := NewChannelMonitorV2Aggregator(repo, channelMonitorRuntimeReaderStub{runtime: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV2}}, nil, nil)
	agg.now = func() time.Time { return now }

	require.NoError(t, agg.Run(context.Background()))
	require.Equal(t, [][2]time.Time{{now.Add(-2 * time.Hour), now}}, repo.ranges)

	now = now.Add(time.Minute)
	require.NoError(t, agg.Run(context.Background()))
	require.Len(t, repo.ranges, 1)

	now = now.Add(4 * time.Minute)
	require.NoError(t, agg.Run(context.Background()))
	require.Len(t, repo.ranges, 3)
	require.Equal(t, [2]time.Time{now.Add(-10 * time.Minute), now}, repo.ranges[1])
	require.Equal(t, [2]time.Time{now.Add(-3*time.Hour - 5*time.Minute), now.Add(-2*time.Hour - 5*time.Minute)}, repo.ranges[2])
}

func TestChannelMonitorV2AggregationWorkerUsesUnifiedRuntime(t *testing.T) {
	repo := &channelMonitorV2AggregatorRepoStub{config: ChannelMonitorV2Config{Enabled: true, RefreshIntervalSeconds: 60}}
	agg := NewChannelMonitorV2Aggregator(repo, channelMonitorRuntimeReaderStub{runtime: ChannelMonitorRuntime{Enabled: true, Mode: ChannelMonitorModeV2}}, nil, nil)

	worker, err := NewChannelMonitorV2AggregationWorker(agg)
	require.NoError(t, err)
	snapshot := worker.Snapshot()
	require.Equal(t, "channel-monitor-v2-aggregation", snapshot.Descriptor.Name)
	require.Equal(t, workerruntime.KindPeriodic, snapshot.Descriptor.Kind)
	require.Equal(t, "monitoring", snapshot.Descriptor.Group)
	require.Equal(t, workerruntime.CoordinationSingletonRun, snapshot.Descriptor.CoordinationMode)
	require.Empty(t, repo.ranges, "adapter construction must not start aggregation")
}
