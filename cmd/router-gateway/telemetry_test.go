package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"weave-os/router/internal/config"
	"weave-os/router/internal/gateway"
	"weave-os/router/internal/observability/otel"
)

func TestGatewayTelemetryExport(t *testing.T) {
	exported := make(chan *coltracepb.ExportTraceServiceRequest, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/traces", r.URL.Path)
		assert.Equal(t, "Bearer synthetic", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var batch coltracepb.ExportTraceServiceRequest
		require.NoError(t, proto.Unmarshal(body, &batch))
		exported <- &batch
		w.WriteHeader(http.StatusNoContent)
	}))
	defer collector.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", collector.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer%20synthetic")
	t.Setenv("OTEL_SERVICE_NAME", "router-gateway")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "deployment.region=us%2Ccentral")
	resourceAttributes, err := otel.ResourceAttributesFromEnvironment(context.Background())
	require.NoError(t, err)
	resourceAttributes["router.deployment_mode"] = "managed"
	exporterHeaders, err := otel.ParseOTLPHeaders(config.GetOr("OTEL_EXPORTER_OTLP_HEADERS", ""))
	require.NoError(t, err)
	emitter, err := otel.NewEmitter(otel.EmitterConfig{
		Endpoint:      config.GetOr("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		Headers:       exporterHeaders,
		ServiceName:   config.GetOr("OTEL_SERVICE_NAME", "router-gateway"),
		ResourceAttrs: resourceAttributes,
	})
	require.NoError(t, err)
	start := time.Now()
	observeGatewayLatency(emitter)(gateway.LatencySample{RequestID: "gateway-id", WorkerRequestID: "worker-id", OrganizationID: "organization", Start: start, End: start.Add(time.Second), StatusCode: 201, Milliseconds: map[gateway.LatencyStage]float64{gateway.LatencySigning: .125, gateway.LatencyConnection: 0, gateway.LatencyDispatch: 7.25, gateway.LatencyFullResponse: 1000.75}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, emitter.Shutdown(ctx))
	batch := <-exported
	require.Len(t, batch.ResourceSpans, 1)
	attrs := func(kvs []*commonv1.KeyValue) map[string]*commonv1.AnyValue {
		values := make(map[string]*commonv1.AnyValue)
		for _, kv := range kvs {
			values[kv.Key] = kv.Value
		}
		return values
	}
	resource := attrs(batch.ResourceSpans[0].Resource.Attributes)
	assert.Equal(t, "router-gateway", resource["service.name"].GetStringValue())
	assert.Equal(t, "managed", resource["router.deployment_mode"].GetStringValue())
	assert.Equal(t, "us,central", resource["deployment.region"].GetStringValue())
	span := batch.ResourceSpans[0].ScopeSpans[0].Spans[0]
	assert.Equal(t, "router.gateway", span.Name)
	values := attrs(span.Attributes)
	assert.Equal(t, "gateway-id", values["request_id"].GetStringValue())
	assert.Equal(t, "worker-id", values["gateway.worker_request_id"].GetStringValue())
	assert.Equal(t, "organization", values["external_id"].GetStringValue())
	assert.Equal(t, int64(201), values["upstream.status_code"].GetIntValue())
	assert.Equal(t, .125, values["latency.gateway_signing_ms"].GetDoubleValue())
	assert.Contains(t, values, "latency.gateway_connection_ms")
	assert.Equal(t, 0.0, values["latency.gateway_connection_ms"].GetDoubleValue())
	assert.Equal(t, 7.25, values["latency.gateway_dispatch_ms"].GetDoubleValue())
	assert.Equal(t, int64(1000), values["latency.full_e2e_ms"].GetIntValue())
	assert.NotContains(t, values, "latency.gateway_tls_ms")
}

func TestGatewayTelemetryDisabled(t *testing.T) {
	observeGatewayLatency(nil)(gateway.LatencySample{})
}

func TestGatewayTelemetryFullAndFailedExportDoesNotBlockObservation(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var exports atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		exports.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(collector.Close)
	emitter, err := otel.NewEmitter(otel.EmitterConfig{Endpoint: collector.URL, Workers: 1, QueueSize: 1, BatchSize: 1, ExportTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() {
		close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, emitter.Shutdown(ctx))
	})
	observe := observeGatewayLatency(emitter)
	sample := gateway.LatencySample{RequestID: "local-only", OrganizationID: "organization", Start: time.Now(), End: time.Now()}
	observe(sample)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("collector did not receive export")
	}
	done := make(chan struct{})
	go func() {
		for range 100 {
			observe(sample)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("telemetry observation blocked on a full queue")
	}
	require.Equal(t, int32(1), exports.Load())
}
