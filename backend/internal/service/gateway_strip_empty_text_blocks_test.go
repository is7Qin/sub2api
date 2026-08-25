//go:build !unit

package service

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestStripEmptyTextBlocks_RawBytesAndExactMatching(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"signed\nvalue","signature":"sig=","opaque":9007199254740993123456789},{"type":"text","text":"one\ttwo"},{"type":"text"},{"type":"text","text":null},{"type":"text","text":7},{"type":"thinking","thinking":"","signature":"keep"},{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"nested keep"},{"type":"text","text":""}]},{"type":"text","text":"seven"},{"type":"text","text":"eight"},{"type":"text","text":""},{"type":"text","text":"ten"},{"type":"redacted_thinking","data":"opaque\/bytes"},{"type":"text","text":""},{"type":"text","text":"thirteen"}]}],"large":1e+100}`)
	before := map[string]string{
		"thinking": gjson.GetBytes(body, "messages.0.content.0").Raw,
		"escaped":  gjson.GetBytes(body, "messages.0.content.1").Raw,
		"nested":   gjson.GetBytes(body, "messages.0.content.6.content.0").Raw,
		"redacted": gjson.GetBytes(body, "messages.0.content.11").Raw,
	}
	normalized := StripEmptyTextBlocks(body)
	require.Len(t, gjson.GetBytes(normalized, "messages.0.content").Array(), 12)
	require.Equal(t, before["thinking"], gjson.GetBytes(normalized, "messages.0.content.0").Raw)
	require.Equal(t, before["escaped"], gjson.GetBytes(normalized, "messages.0.content.1").Raw)
	require.Equal(t, before["nested"], gjson.GetBytes(normalized, "messages.0.content.6.content.0").Raw)
	require.Equal(t, before["redacted"], gjson.GetBytes(normalized, "messages.0.content.10").Raw)
	require.Equal(t, "1e+100", gjson.GetBytes(normalized, "large").Raw)
	require.True(t, bytes.Equal(normalized, StripEmptyTextBlocks(normalized)))

	noMatch := []byte(`{"messages":[{"content":[{"type":"text"},{"type":"text","text":null},{"type":"text","text":0},{"type":"thinking","thinking":""},{"type":"text","text":" "}]}]}`)
	require.Equal(t, noMatch, StripEmptyTextBlocks(noMatch))
	malformed := []byte(`{"messages":[`)
	require.Equal(t, malformed, StripEmptyTextBlocks(malformed))
}
