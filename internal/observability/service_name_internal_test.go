package observability

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// serviceName is what puts the "name" tag on every line. NAME is set per
// service by the deployment; the fallbacks keep output tagged anywhere else.
func TestServiceNamePrefersNAMEThenOTELThenDefault(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		expects string
	}{
		{"NAME wins", map[string]string{"NAME": "router", "OTEL_SERVICE_NAME": "other", "OTEL_RESOURCE_ATTRIBUTES": "service.name=resource-name"}, "router"},
		{"OTEL fallback", map[string]string{"OTEL_SERVICE_NAME": "router-hmm-sidecar"}, "router-hmm-sidecar"},
		{"OTEL service name wins over resource attribute", map[string]string{"OTEL_SERVICE_NAME": "otel-name", "OTEL_RESOURCE_ATTRIBUTES": "service.name=resource-name"}, "otel-name"},
		{"resource attribute fallback", map[string]string{"OTEL_RESOURCE_ATTRIBUTES": "service.name=router-resource"}, "router-resource"},
		{"default when unset", nil, defaultServiceName},
		{"whitespace-only is not a name", map[string]string{"NAME": "   "}, defaultServiceName},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NAME", "")
			t.Setenv("OTEL_SERVICE_NAME", "")
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			assert.Equal(t, tc.expects, serviceName())
		})
	}
}

func TestResourceAttributesFromEnvironmentSanitizesParseErrors(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "service.name=router,do-not-log-this-secret")

	attributes, err := ResourceAttributesFromEnvironment(context.Background())
	require.ErrorIs(t, err, errInvalidResourceAttributes)
	assert.NotContains(t, err.Error(), "do-not-log-this-secret")
	assert.Equal(t, "router", attributes["service.name"])
}

// buildLogger is what initLogger installs, so this fails if the service tag
// is ever dropped from it. Asserting on a hand-built logger instead would
// restate the logic and stay green through that regression.
func TestBuildLoggerAttachesServiceTag(t *testing.T) {
	t.Setenv("NAME", "router-test-svc")

	var buf strings.Builder
	buildLogger(slog.NewJSONHandler(&buf, nil)).Info("hello")

	assert.Contains(t, buf.String(), `"name":"router-test-svc"`)
}

// resolveLevel gates whether Debug lines survive at all.
func TestResolveLevelFromEnv(t *testing.T) {
	for env, want := range map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelInfo,
		"bogus":   slog.LevelInfo,
	} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", env)
			assert.Equal(t, want, resolveLevel())
		})
	}
}
