package service

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/modelcatalog"
)

const SupportDecisionHotModelLimit = 32

// SupportDecisionModelCatalog separates the bounded hot-table catalog from
// normalized overflow entries that later fallback classification must retain.
type SupportDecisionModelCatalog struct {
	HotModels      []string
	FallbackModels []string
}

// ResolveSupportDecisionHotModels resolves group, process configuration, and
// platform defaults in precedence order, then applies the bounded hot-table cap.
func ResolveSupportDecisionHotModels(platform string, group *Group, configured config.SupportDecisionHotModelsConfig) SupportDecisionModelCatalog {
	var models []string
	if group != nil && group.ModelsListConfig.Enabled && len(group.ModelsListConfig.Models) > 0 {
		models = group.ModelsListConfig.Models
	} else if configuredModels := configured.ForPlatform(platform); len(configuredModels) > 0 {
		models = configuredModels
	} else {
		models = modelcatalog.DefaultModelIDs(platform)
	}

	normalized := normalizeSupportDecisionModels(models)
	hotCount := min(len(normalized), SupportDecisionHotModelLimit)
	return SupportDecisionModelCatalog{
		HotModels:      normalized[:hotCount:hotCount],
		FallbackModels: normalized[hotCount:],
	}
}

func normalizeSupportDecisionModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	normalized := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		normalized = append(normalized, model)
	}
	return normalized
}
