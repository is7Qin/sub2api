package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/modelcatalog"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGatewayDefaultModelCatalogUsesSharedAdapter(t *testing.T) {
	for _, platform := range []string{
		service.PlatformOpenAI,
		service.PlatformAnthropic,
		service.PlatformGemini,
		service.PlatformAntigravity,
	} {
		t.Run(platform, func(t *testing.T) {
			require.Equal(t, modelcatalog.DefaultModelIDs(platform), defaultModelIDsForPlatform(platform))
		})
	}
}
