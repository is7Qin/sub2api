// Package modelcatalog exposes dependency-neutral adapters over the platform
// packages' existing public default model catalogs.
package modelcatalog

import (
	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// DefaultModelIDs returns a new slice containing the existing default model IDs
// for platform. Unknown and empty platforms retain the historical Anthropic default.
func DefaultModelIDs(platform string) []string {
	switch platform {
	case domain.PlatformOpenAI:
		return openai.DefaultModelIDs()
	case domain.PlatformGemini:
		ids := make([]string, 0, len(geminicli.DefaultModels))
		for _, model := range geminicli.DefaultModels {
			ids = append(ids, model.ID)
		}
		return ids
	case domain.PlatformAntigravity:
		models := antigravity.DefaultModels()
		ids := make([]string, 0, len(models))
		for _, model := range models {
			ids = append(ids, model.ID)
		}
		return ids
	default:
		return claude.DefaultModelIDs()
	}
}
