package observability

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
)

var errInvalidResourceAttributes = errors.New("OTEL_RESOURCE_ATTRIBUTES contains invalid entries")

// ResourceAttributesFromEnvironment returns string resource attributes parsed
// by the OpenTelemetry SDK, including service.name. Parse errors are sanitized
// because the SDK error can contain the original environment value.
func ResourceAttributesFromEnvironment(ctx context.Context) (map[string]string, error) {
	resource, err := sdkresource.New(ctx, sdkresource.WithFromEnv())
	attributes := make(map[string]string)
	if resource != nil {
		for _, resourceAttribute := range resource.Attributes() {
			if resourceAttribute.Value.Type() != attribute.STRING {
				continue
			}
			attributes[string(resourceAttribute.Key)] = resourceAttribute.Value.AsString()
		}
	}
	if err != nil {
		return attributes, errInvalidResourceAttributes
	}
	return attributes, nil
}
