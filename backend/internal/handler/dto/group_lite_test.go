package dto

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

// TestGroupSerializationIncludesZeroValues 验证非 admin 接口的 Group 序列化
// 契约（P1-7 回归）：零值字段（false/0/空串）与 nil 指针字段必须出现，
// 不允许经 omitempty 从响应中消失。
func TestGroupSerializationIncludesZeroValues(t *testing.T) {
	g := GroupFromService(&service.Group{ID: 5, Name: "g"})
	raw, err := json.Marshal(g)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))

	require.Equal(t, float64(5), m["id"])
	require.Equal(t, "g", m["name"])
	// 零值字段必须显式出现
	require.Contains(t, m, "is_exclusive")
	require.Equal(t, false, m["is_exclusive"])
	require.Contains(t, m, "description")
	require.Equal(t, "", m["description"])
	require.Contains(t, m, "rate_multiplier")
	require.Equal(t, float64(0), m["rate_multiplier"])
	require.Contains(t, m, "status")
	require.Equal(t, "", m["status"])
	// nil 指针字段序列化为 null 而非省略
	require.Contains(t, m, "image_price_1k")
	require.Nil(t, m["image_price_1k"])
	// created_at/updated_at 以时间字符串出现（time.Time 零值显式输出）
	require.Contains(t, m, "created_at")
	require.Equal(t, "0001-01-01T00:00:00Z", m["created_at"])
	require.Contains(t, m, "updated_at")
	require.Equal(t, "0001-01-01T00:00:00Z", m["updated_at"])
}

// TestGroupLiteSerializesOnlyLiteFields 验证 GroupLite 只输出列表 UI 消费的
// 5 个字段：非零值出现、零值省略、其余字段（即使非零）绝不出现。
func TestGroupLiteSerializesOnlyLiteFields(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	g := GroupLiteFromService(&service.Group{
		ID:               7,
		Name:             "g",
		Description:      "desc",
		Platform:         "openai",
		RateMultiplier:   1.5,
		IsExclusive:      true,
		Status:           "active",
		SubscriptionType: "plus",
		CreatedAt:        now,
		UpdatedAt:        now,
	})
	raw, err := json.Marshal(g)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, map[string]any{
		"id":                float64(7),
		"name":              "g",
		"platform":          "openai",
		"subscription_type": "plus",
		"rate_multiplier":   1.5,
	}, m)

	// 零值字段全部省略
	zero := GroupLiteFromService(&service.Group{ID: 1})
	rawZero, err := json.Marshal(zero)
	require.NoError(t, err)
	var mZero map[string]any
	require.NoError(t, json.Unmarshal(rawZero, &mZero))
	require.Equal(t, map[string]any{"id": float64(1)}, mZero)
}

// TestAccountFromServiceGroupsAreLite 验证 Account DTO 的 groups 走 GroupLite
// 投影：即使 service 层携带完整 group 对象（含描述/专属标记/时间），响应中也
// 只出现 5 个列表字段。
func TestAccountFromServiceGroupsAreLite(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	src := &service.Account{
		ID:   1,
		Name: "acc",
		Groups: []*service.Group{
			{
				ID:               7,
				Name:             "g",
				Description:      "desc",
				Platform:         "openai",
				RateMultiplier:   1.5,
				IsExclusive:      true,
				Status:           "active",
				SubscriptionType: "plus",
				CreatedAt:        now,
				UpdatedAt:        now,
			},
		},
	}

	got := AccountFromService(src)
	raw, err := json.Marshal(got)
	require.NoError(t, err)

	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	groups, ok := m["groups"].([]any)
	require.True(t, ok, "groups must be present in account payload")
	require.Len(t, groups, 1)

	groupMap, ok := groups[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{
		"id":                float64(7),
		"name":              "g",
		"platform":          "openai",
		"subscription_type": "plus",
		"rate_multiplier":   1.5,
	}, groupMap)
}
