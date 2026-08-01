//go:build unit

package service

import (
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAnthropicSSEFieldValue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		line  string
		field string
		want  string
		ok    bool
	}{
		{name: "standard event", line: "event: message_start", field: "event", want: "message_start", ok: true},
		{name: "compact event", line: "event:message_start", field: "event", want: "message_start", ok: true},
		{name: "standard data", line: `data: {"type":"message_stop"}`, field: "data", want: `{"type":"message_stop"}`, ok: true},
		{name: "compact data", line: `data:{"type":"message_stop"}`, field: "data", want: `{"type":"message_stop"}`, ok: true},
		{name: "one optional space only", line: "data:  value", field: "data", want: " value", ok: true},
		{name: "tab is value", line: "data:	value", field: "data", want: "	value", ok: true},
		{name: "empty value", line: "event:", field: "event", want: "", ok: true},
		{name: "single-space value", line: "data: ", field: "data", want: "", ok: true},
		{name: "wrong field", line: "id: message_start", field: "event", ok: false},
		{name: "field prefix only", line: "eventual: message_start", field: "event", ok: false},
		{name: "missing colon", line: "event message_start", field: "event", ok: false},
		{name: "empty line", line: "", field: "event", ok: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := anthropicSSEFieldValue(tc.line, tc.field)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.want, got)
		})
	}
}

// oneByteReader proves scanner behavior is independent of transport read splits.
type oneByteReader struct {
	r io.Reader
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.r.Read(p)
}
