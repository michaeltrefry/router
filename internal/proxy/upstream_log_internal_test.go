package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogUpstreamBody_KeepsBoundSessionKey(t *testing.T) {
	var buf bytes.Buffer
	ctx := observability.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
	env, err := translate.ParseAnthropic([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)

	_, log, key := bindRequestLogger(ctx, env, "api-key", "req-1", "anthropic_messages")
	want := shortKey(key)
	require.NotEmpty(t, want)

	logUpstreamBody(log, router.Decision{Model: "m", Provider: providers.ProviderAnthropic}, translate.RoutingFeatures{MessageCount: 1}, []byte("{}"))

	var line string
	sc := bufio.NewScanner(&buf)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"msg":"upstream prepared request"`) {
			line = sc.Text()
		}
	}
	require.NotEmpty(t, line, "upstream prepared request line not emitted")

	assert.Equal(t, 1, strings.Count(line, `"session_key":`), "session_key must appear once: %s", line)
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &rec))
	assert.Equal(t, want, rec["session_key"])
}
