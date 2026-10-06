package otel

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResourceAttributesFromEnvironment(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.region=us%2Ccentral,build.channel=stable%2Bcanary")
	t.Setenv("OTEL_SERVICE_NAME", "router-gateway")

	attributes, err := ResourceAttributesFromEnvironment(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "us,central", attributes["deployment.region"])
	assert.Equal(t, "stable+canary", attributes["build.channel"])
	assert.NotContains(t, attributes, "service.name")
}

func TestParseOTLPHeaders(t *testing.T) {
	headers, err := ParseOTLPHeaders("Authorization=Bearer%20synthetic,X-Details=comma%2Cequals%3Dplus%2Bspace%20here")
	require.NoError(t, err)
	assert.Equal(t, "Bearer synthetic", headers["Authorization"])
	assert.Equal(t, "comma,equals=plus+space here", headers["X-Details"])
}

func TestParseOTLPHeadersKeepsValidEntriesOnMalformedValue(t *testing.T) {
	headers, err := ParseOTLPHeaders("X-Valid=works,Authorization=%ZZ")
	require.Error(t, err)
	assert.Equal(t, "works", headers["X-Valid"])
}
