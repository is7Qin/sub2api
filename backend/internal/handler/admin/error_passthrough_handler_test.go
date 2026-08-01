package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type errorPassthroughHandlerRepo struct {
	rule *model.ErrorPassthroughRule
}

func (r *errorPassthroughHandlerRepo) List(context.Context) ([]*model.ErrorPassthroughRule, error) {
	if r.rule == nil {
		return []*model.ErrorPassthroughRule{}, nil
	}
	return []*model.ErrorPassthroughRule{r.rule}, nil
}

func (r *errorPassthroughHandlerRepo) GetByID(_ context.Context, id int64) (*model.ErrorPassthroughRule, error) {
	if r.rule == nil || r.rule.ID != id {
		return nil, nil
	}
	return r.rule, nil
}

func (r *errorPassthroughHandlerRepo) Create(_ context.Context, rule *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	r.rule = rule
	return r.rule, nil
}

func (r *errorPassthroughHandlerRepo) Update(_ context.Context, rule *model.ErrorPassthroughRule) (*model.ErrorPassthroughRule, error) {
	r.rule = rule
	return r.rule, nil
}

func (r *errorPassthroughHandlerRepo) Delete(_ context.Context, id int64) error {
	if r.rule != nil && r.rule.ID == id {
		r.rule = nil
	}
	return nil
}

func setupErrorPassthroughRuleRouter(repo *errorPassthroughHandlerRepo) *gin.Engine {
	handler := NewErrorPassthroughHandler(service.NewErrorPassthroughService(repo, nil))
	router := gin.New()
	router.PUT("/api/v1/admin/error-passthrough-rules/:id", handler.Update)
	router.GET("/api/v1/admin/error-passthrough-rules/:id", handler.GetByID)
	return router
}

func TestErrorPassthroughHandlerUpdateDescriptionPatchSemantics(t *testing.T) {
	for _, tt := range []struct {
		name            string
		payload         string
		wantDescription *string
	}{
		{name: "explicit null clears", payload: `{"description":null}`, wantDescription: nil},
		{name: "omitted preserves", payload: `{}`, wantDescription: stringPtr("stale description")},
		{name: "empty string persists", payload: `{"description":""}`, wantDescription: stringPtr("")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			description := "stale description"
			repo := &errorPassthroughHandlerRepo{rule: &model.ErrorPassthroughRule{
				ID:              1,
				Name:            "upstream overload",
				Enabled:         true,
				ErrorCodes:      []int{http.StatusServiceUnavailable},
				Keywords:        []string{},
				MatchMode:       model.MatchModeAny,
				Platforms:       []string{},
				PassthroughCode: true,
				PassthroughBody: true,
				Description:     &description,
			}}
			router := setupErrorPassthroughRuleRouter(repo)

			updateRec := httptest.NewRecorder()
			updateReq := httptest.NewRequest(
				http.MethodPut,
				"/api/v1/admin/error-passthrough-rules/1",
				bytes.NewBufferString(tt.payload),
			)
			updateReq.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(updateRec, updateReq)
			require.Equal(t, http.StatusOK, updateRec.Code)

			getRec := httptest.NewRecorder()
			getReq := httptest.NewRequest(http.MethodGet, "/api/v1/admin/error-passthrough-rules/1", nil)
			router.ServeHTTP(getRec, getReq)
			require.Equal(t, http.StatusOK, getRec.Code)

			var result struct {
				Data model.ErrorPassthroughRule `json:"data"`
			}
			require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &result))
			require.Equal(t, tt.wantDescription, result.Data.Description)
		})
	}
}

func stringPtr(value string) *string {
	return &value
}
