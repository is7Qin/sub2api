package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIStreamEventIsTerminalWithTypeMatchesExistingSemantics(t *testing.T) {
	tests := []struct {
		name string
		data string
		want bool
	}{
		{name: "empty", data: "", want: false},
		{name: "whitespace", data: " \t ", want: false},
		{name: "done", data: " [DONE] ", want: true},
		{name: "completed", data: `{"type":"response.completed"}`, want: true},
		{name: "failed", data: `{"type":"response.failed"}`, want: true},
		{name: "delta", data: `{"type":"response.output_text.delta"}`, want: false},
		{name: "invalid JSON", data: `{"type":`, want: false},
		{name: "terminal with trailing garbage", data: `{"type":"response.completed"} trailing`, want: true},
		{name: "type whitespace remains nonterminal", data: `{"type":" response.completed "}`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eventType := gjson.GetBytes([]byte(tt.data), "type").String()
			got := openAIStreamEventIsTerminalWithType(tt.data, eventType)

			require.Equal(t, tt.want, got)
			require.Equal(t, openAIStreamEventIsTerminal(tt.data), got)
		})
	}
}

var (
	benchmarkOpenAIStreamEventTypeSink string
	benchmarkOpenAIStreamTerminalSink  bool
)

func BenchmarkOpenAIStreamTerminalTypeReuse(b *testing.B) {
	data := `{"type":"response.output_text.delta","sequence_number":42,"delta":"streaming response benchmark payload"}`
	dataBytes := []byte(data)

	b.Run("double parse", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(dataBytes)))
		for i := 0; i < b.N; i++ {
			benchmarkOpenAIStreamTerminalSink = openAIStreamEventIsTerminal(data)
			benchmarkOpenAIStreamEventTypeSink = strings.TrimSpace(gjson.GetBytes(dataBytes, "type").String())
		}
	})

	b.Run("reused type", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(dataBytes)))
		for i := 0; i < b.N; i++ {
			eventType := strings.TrimSpace(gjson.GetBytes(dataBytes, "type").String())
			benchmarkOpenAIStreamEventTypeSink = eventType
			benchmarkOpenAIStreamTerminalSink = openAIStreamEventIsTerminalWithType(data, eventType)
		}
	})
}
