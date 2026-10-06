package otel

import (
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"net/url"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	"golang.org/x/net/http/httpguts"
)

var errInvalidOTLPHeaders = errors.New("OTLP header environment contains invalid entries")

// ResourceAttributesFromEnvironment returns string resource attributes parsed
// by the OpenTelemetry SDK, including service.name.
func ResourceAttributesFromEnvironment(ctx context.Context) (map[string]string, error) {
	resource, err := sdkresource.New(ctx, sdkresource.WithFromEnv())
	attributes := make(map[string]string)
	if resource == nil {
		return attributes, err
	}
	for _, resourceAttribute := range resource.Attributes() {
		if resourceAttribute.Value.Type() != attribute.STRING {
			continue
		}
		attributes[string(resourceAttribute.Key)] = resourceAttribute.Value.AsString()
	}
	return attributes, err
}

// ResolveServiceName chooses the explicit service name, then the resource
// attribute, then the caller's default.
func ResolveServiceName(explicitName string, resourceAttributes map[string]string, fallback string) string {
	if explicitName != "" {
		return explicitName
	}
	if resourceName := resourceAttributes["service.name"]; resourceName != "" {
		return resourceName
	}
	return fallback
}

// ParseOTLPHeaders parses the OpenTelemetry OTLP exporter header environment
// value, including URL-encoded values, into HTTP headers. Invalid entries are
// omitted while valid entries remain available to the exporter.
func ParseOTLPHeaders(raw string) (map[string]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	entries := strings.Split(raw, ",")
	headers := make(map[string]string, len(entries))
	hasInvalidEntries := false
	var firstInvalidEntryError error
	for _, entry := range entries {
		name, value, ok := strings.Cut(strings.TrimSpace(entry), "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			hasInvalidEntries = true
			if firstInvalidEntryError == nil {
				firstInvalidEntryError = errors.New("malformed OTLP exporter header entry")
			}
			continue
		}
		if !httpguts.ValidHeaderFieldName(name) {
			hasInvalidEntries = true
			if firstInvalidEntryError == nil {
				firstInvalidEntryError = fmt.Errorf("invalid OTLP exporter header name %q", name)
			}
			continue
		}

		decodedValue, err := url.PathUnescape(strings.TrimSpace(value))
		if err != nil || !httpguts.ValidHeaderFieldValue(decodedValue) {
			hasInvalidEntries = true
			if firstInvalidEntryError == nil {
				firstInvalidEntryError = fmt.Errorf("invalid OTLP exporter header value for %q", name)
			}
			continue
		}
		headers[textproto.CanonicalMIMEHeaderKey(name)] = decodedValue
	}
	if hasInvalidEntries {
		return headers, errors.Join(errInvalidOTLPHeaders, firstInvalidEntryError)
	}
	return headers, nil
}
