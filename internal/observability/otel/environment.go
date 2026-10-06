package otel

import (
	"context"
	"errors"
	"net/textproto"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

var errInvalidOTLPHeaders = errors.New("OTLP header environment contains invalid entries")

// ResourceAttributesFromEnvironment returns string resource attributes parsed
// by the OpenTelemetry SDK. Service name is configured separately by EmitterConfig.
func ResourceAttributesFromEnvironment(ctx context.Context) (map[string]string, error) {
	resource, err := sdkresource.New(ctx, sdkresource.WithFromEnv())
	attributes := make(map[string]string)
	if resource == nil {
		return attributes, err
	}
	for _, resourceAttribute := range resource.Attributes() {
		if resourceAttribute.Key == attribute.Key("service.name") || resourceAttribute.Value.Type() != attribute.STRING {
			continue
		}
		attributes[string(resourceAttribute.Key)] = resourceAttribute.Value.AsString()
	}
	return attributes, err
}

// ParseOTLPHeaders parses the OpenTelemetry OTLP exporter header environment
// value, including URL-encoded values, into HTTP headers.
func ParseOTLPHeaders(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	query := strings.ReplaceAll(strings.ReplaceAll(raw, "+", "%2B"), ",", "&")
	values, err := url.ParseQuery(query)
	headers := make(map[string]string, len(values))
	invalid := err != nil
	for name, candidates := range values {
		headerName := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(name))
		if headerName == "" || len(candidates) == 0 {
			invalid = true
			continue
		}
		headers[headerName] = strings.TrimSpace(candidates[len(candidates)-1])
	}
	if invalid {
		return headers, errInvalidOTLPHeaders
	}
	return headers, nil
}
