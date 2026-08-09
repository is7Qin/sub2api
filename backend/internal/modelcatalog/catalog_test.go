package modelcatalog

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/pkg/antigravity"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/geminicli"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/stretchr/testify/require"
)

func TestDefaultModelCatalogCoversAllFourPlatforms(t *testing.T) {
	antigravityModels := antigravity.DefaultModels()
	antigravityIDs := make([]string, 0, len(antigravityModels))
	for _, model := range antigravityModels {
		antigravityIDs = append(antigravityIDs, model.ID)
	}
	geminiIDs := make([]string, 0, len(geminicli.DefaultModels))
	for _, model := range geminicli.DefaultModels {
		geminiIDs = append(geminiIDs, model.ID)
	}

	tests := []struct {
		platform string
		want     []string
	}{
		{platform: domain.PlatformOpenAI, want: openai.DefaultModelIDs()},
		{platform: domain.PlatformAnthropic, want: claude.DefaultModelIDs()},
		{platform: domain.PlatformGemini, want: geminiIDs},
		{platform: domain.PlatformAntigravity, want: antigravityIDs},
	}

	for _, test := range tests {
		t.Run(test.platform, func(t *testing.T) {
			require.Equal(t, test.want, DefaultModelIDs(test.platform))
		})
	}
}
