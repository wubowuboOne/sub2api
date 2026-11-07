package config

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOpenAIStreamRetryConfiguration(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		resetViperWithJWTSecret(t)
		t.Setenv("GATEWAY_OPENAI_STREAM_RETRY_MAX_LINE_BYTES", "")
		t.Setenv("GATEWAY_OPENAI_STREAM_RETRY_MAX_BUFFER_BYTES", "")
		c, e := Load()
		require.NoError(t, e)
		require.Equal(t, DefaultOpenAIStreamRetryMaxLineBytes, c.Gateway.OpenAIStreamRetryMaxLineBytes)
		require.EqualValues(t, DefaultOpenAIStreamRetryMaxBufferBytes, c.Gateway.OpenAIStreamRetryMaxBufferBytes)
	})
	for _, tc := range []struct{ name, line, buffer, want string }{
		{"zero defaults", "0", "0", ""},
		{"explicit", "16777216", "33554432", ""},
		{"negative line", "-1", "0", "max_line_bytes"},
		{"small line", "1024", "0", "max_line_bytes"},
		{"large line", "134217729", "0", "max_line_bytes"},
		{"negative buffer", "0", "-1", "max_buffer_bytes"},
		{"small buffer", "0", "1024", "max_buffer_bytes"},
		{"large buffer", "0", "536870913", "max_buffer_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("GATEWAY_OPENAI_STREAM_RETRY_MAX_LINE_BYTES", tc.line)
			t.Setenv("GATEWAY_OPENAI_STREAM_RETRY_MAX_BUFFER_BYTES", tc.buffer)
			c, e := Load()
			if tc.want == "" {
				require.NoError(t, e)
				if tc.name == "explicit" {
					require.Equal(t, 16*1024*1024, c.Gateway.OpenAIStreamRetryMaxLineBytes)
					require.EqualValues(t, 32*1024*1024, c.Gateway.OpenAIStreamRetryMaxBufferBytes)
				}
			} else {
				require.ErrorContains(t, e, tc.want)
			}
		})
	}
}
