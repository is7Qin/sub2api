package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type keyBillingUserGroupRateRepo struct {
	service.UserGroupRateRepository
	rate        *float64
	gotUserID   int64
	gotGroupID  int64
	lookupCalls int
}

func (r *keyBillingUserGroupRateRepo) GetByUserAndGroup(_ context.Context, userID, groupID int64) (*float64, error) {
	r.gotUserID = userID
	r.gotGroupID = groupID
	r.lookupCalls++
	return r.rate, nil
}

func newKeyBillingHandler(repo service.UserGroupRateRepository) *GatewayHandler {
	cfg := &config.Config{}
	gatewayService := service.NewGatewayService(
		nil, nil, nil, nil, nil, nil, repo, nil, cfg, nil, nil, nil, nil, nil,
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	return &GatewayHandler{gatewayService: gatewayService}
}

func newKeyBillingContext(apiKey *service.APIKey) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/sub2api/billing", nil)
	if apiKey != nil {
		c.Set(string(middleware2.ContextKeyAPIKey), apiKey)
	}
	return c, w
}

func TestGatewayHandlerKeyBillingInfoUsesGroupRate(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{
		UserID: 11, GroupID: &groupID, Key: "sensitive-key-value",
		Group: &service.Group{ID: groupID, Name: "private-group-name", RateMultiplier: 0.75},
	}
	c, w := newKeyBillingContext(apiKey)

	newKeyBillingHandler(nil).KeyBillingInfo(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	var got keyBillingInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, "sub2api.key_billing", got.Object)
	require.Equal(t, keyBillingInfoSchemaVersion, got.SchemaVersion)
	require.Equal(t, "token", got.BillingScope)
	require.Equal(t, 0.75, got.GroupRateMultiplier)
	require.Nil(t, got.UserRateMultiplier)
	require.Equal(t, 0.75, got.ResolvedRateMultiplier)
	require.False(t, got.PeakRateEnabled)
	require.Nil(t, got.PeakStart)
	require.Nil(t, got.PeakEnd)
	require.Nil(t, got.PeakRateMultiplier)
	require.Nil(t, got.AppliedPeakMultiplier)
	require.Equal(t, 0.75, got.EffectiveRateMultiplier)
	require.Nil(t, got.Timezone)
	require.WithinDuration(t, time.Now().UTC(), got.ObservedAt, time.Second)
	require.NotContains(t, w.Body.String(), apiKey.Key)
	require.NotContains(t, w.Body.String(), apiKey.Group.Name)
}

func TestGatewayHandlerKeyBillingInfoUsesUserOverride(t *testing.T) {
	groupID := int64(7)
	userRate := 0.5
	apiKey := &service.APIKey{
		UserID: 11, GroupID: &groupID,
		Group: &service.Group{ID: groupID, RateMultiplier: 0.75},
	}
	c, w := newKeyBillingContext(apiKey)
	repo := &keyBillingUserGroupRateRepo{rate: &userRate}

	newKeyBillingHandler(repo).KeyBillingInfo(c)

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, 1, repo.lookupCalls)
	require.Equal(t, apiKey.UserID, repo.gotUserID)
	require.Equal(t, groupID, repo.gotGroupID)
	var got keyBillingInfoResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.NotNil(t, got.UserRateMultiplier)
	require.Equal(t, userRate, *got.UserRateMultiplier)
	require.Equal(t, userRate, got.ResolvedRateMultiplier)
	require.Equal(t, userRate, got.EffectiveRateMultiplier)
}

func TestGatewayHandlerKeyBillingInfoErrorsAreSafe(t *testing.T) {
	t.Run("missing API key", func(t *testing.T) {
		c, w := newKeyBillingContext(nil)
		newKeyBillingHandler(nil).KeyBillingInfo(c)
		require.Equal(t, http.StatusUnauthorized, w.Code)
	})
	t.Run("simple mode", func(t *testing.T) {
		groupID := int64(7)
		c, w := newKeyBillingContext(&service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID}})
		h := newKeyBillingHandler(nil)
		h.cfg = &config.Config{RunMode: config.RunModeSimple}
		h.KeyBillingInfo(c)
		require.Equal(t, http.StatusNotFound, w.Code)
	})
	t.Run("ungrouped API key", func(t *testing.T) {
		c, w := newKeyBillingContext(&service.APIKey{})
		newKeyBillingHandler(nil).KeyBillingInfo(c)
		require.Equal(t, http.StatusForbidden, w.Code)
	})
	t.Run("missing group object", func(t *testing.T) {
		groupID := int64(7)
		c, w := newKeyBillingContext(&service.APIKey{GroupID: &groupID})
		newKeyBillingHandler(nil).KeyBillingInfo(c)
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})
	t.Run("missing billing service", func(t *testing.T) {
		groupID := int64(7)
		c, w := newKeyBillingContext(&service.APIKey{UserID: 11, GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 1}})
		(&GatewayHandler{}).KeyBillingInfo(c)
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})
}

func TestBuildKeyBillingInfoLeavesPeakFieldsUnavailable(t *testing.T) {
	groupID := int64(7)
	apiKey := &service.APIKey{GroupID: &groupID, Group: &service.Group{ID: groupID, RateMultiplier: 1.2}}
	now := time.Date(2026, time.July, 12, 10, 0, 0, 0, time.UTC)

	got := buildKeyBillingInfo(apiKey, 0.8, now)

	require.False(t, got.PeakRateEnabled)
	require.Nil(t, got.PeakStart)
	require.Nil(t, got.PeakEnd)
	require.Nil(t, got.PeakRateMultiplier)
	require.Nil(t, got.AppliedPeakMultiplier)
	require.Nil(t, got.Timezone)
	require.Equal(t, 0.8, got.EffectiveRateMultiplier)
	require.Equal(t, now.UTC(), got.ObservedAt)
}
