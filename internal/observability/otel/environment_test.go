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
	assert.Equal(t, "router-gateway", attributes["service.name"])
}

func TestResolveServiceName(t *testing.T) {
	resourceAttributes := map[string]string{"service.name": "resource-name"}

	assert.Equal(t, "explicit-name", ResolveServiceName("explicit-name", resourceAttributes, "default-name"))
	assert.Equal(t, "resource-name", ResolveServiceName("", resourceAttributes, "default-name"))
	assert.Equal(t, "default-name", ResolveServiceName("", nil, "default-name"))
}

func TestParseOTLPHeaders(t *testing.T) {
	headers, err := ParseOTLPHeaders("Authorization=Bearer%20synthetic,X-Details=comma%2Cequals%3Dplus%2Bspace%20here,X-Raw=ab&cd;ef+gh")
	require.NoError(t, err)
	assert.Equal(t, "Bearer synthetic", headers["Authorization"])
	assert.Equal(t, "comma,equals=plus+space here", headers["X-Details"])
	assert.Equal(t, "ab&cd;ef+gh", headers["X-Raw"])
}

func TestParseOTLPHeadersKeepsValidEntriesOnMalformedValue(t *testing.T) {
	headers, err := ParseOTLPHeaders("X-Valid=works,Authorization=%ZZ")
	require.Error(t, err)
	require.ErrorIs(t, err, errInvalidOTLPHeaders)
	assert.Contains(t, err.Error(), `"Authorization"`)
	assert.NotContains(t, err.Error(), "%ZZ")
	assert.Equal(t, "works", headers["X-Valid"])
}

func TestParseOTLPHeadersRejectsInvalidHeaderNames(t *testing.T) {
	headers, err := ParseOTLPHeaders("Bad Header=private-value")
	require.ErrorIs(t, err, errInvalidOTLPHeaders)
	assert.Contains(t, err.Error(), `"Bad Header"`)
	assert.NotContains(t, err.Error(), "private-value")
	assert.Empty(t, headers)
}

func TestParseOTLPHeadersRejectsInvalidHeaderValues(t *testing.T) {
	headers, err := ParseOTLPHeaders("X-Invalid=one%0Dtwo")
	require.ErrorIs(t, err, errInvalidOTLPHeaders)
	assert.Contains(t, err.Error(), `"X-Invalid"`)
	assert.Empty(t, headers)
}
