package main

import (
	"strings"

	"weave-os/router/internal/config"
	"weave-os/router/internal/gateway"
	"weave-os/router/internal/observability/otel"
)

func buildGatewayEmitter() (*otel.Emitter, error) {
	resourceAttrs := gatewayOtelPairs(config.GetOr("OTEL_RESOURCE_ATTRIBUTES", ""))
	resourceAttrs["router.deployment_mode"] = "managed"
	return otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      config.GetOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		Headers:       gatewayOtelPairs(config.GetOr("OTEL_EXPORTER_OTLP_HEADERS", "")),
		ServiceName:   config.GetOr("OTEL_SERVICE_NAME", "router-gateway"),
		ResourceAttrs: resourceAttrs,
	})
}

func gatewayOtelPairs(raw string) map[string]string {
	pairs := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.TrimSpace(key) != "" {
			pairs[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return pairs
}

func observeGatewayLatency(emitter *otel.Emitter) func(gateway.LatencySample) {
	return func(sample gateway.LatencySample) {
		attrs := otel.NewAttrBuilder(22).
			String("request_id", sample.RequestID).
			String("external_id", sample.OrganizationID)
		if sample.WorkerRequestID != "" {
			attrs.String("gateway.worker_request_id", sample.WorkerRequestID)
		}
		if sample.StatusCode != 0 {
			attrs.Int64("upstream.status_code", int64(sample.StatusCode))
		}
		for stage, milliseconds := range sample.Milliseconds {
			if stage == gateway.LatencyFullResponse {
				attrs.Int64("latency.full_e2e_ms", int64(milliseconds))
			} else {
				attrs.Float64("latency."+string(stage)+"_ms", milliseconds)
			}
		}
		buffer := emitter.NewBuffer()
		buffer.Record(otel.Span{Name: "router.gateway", Start: sample.Start, End: sample.End, Attrs: attrs.Build()})
		buffer.Flush()
	}
}
