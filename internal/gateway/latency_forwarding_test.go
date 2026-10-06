package gateway_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/gateway"
)

func TestGatewayLatencySeparatesDispatchFromWorkerAndStream(t *testing.T) {
	worker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, `{}`, string(body))
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("X-Request-Id", "selected-worker-id")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(60 * time.Millisecond)
		_, _ = io.WriteString(w, "data: last\n\n")
	}))
	defer worker.Close()
	forwarder, _, _ := gatewayFixture(t, worker, nil, nil)
	var samples []gateway.LatencySample
	forwarder.SetLatencyObserver(func(sample gateway.LatencySample) { samples = append(samples, sample) })
	var originalTraceCalls atomic.Int32
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
		r.Header.Set("X-Request-Id", "spoofed-id")
		r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { originalTraceCalls.Add(1) }}))
		w := httptest.NewRecorder()
		forwarder.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code)
		assert.Equal(t, "data: first\n\ndata: last\n\n", w.Body.String())
		assert.Equal(t, "selected-worker-id", w.Header().Get("X-Request-Id"))
	}
	require.Len(t, samples, 2)
	assert.Equal(t, int32(2), originalTraceCalls.Load())
	assert.NotEqual(t, samples[0].RequestID, samples[1].RequestID)
	for _, sample := range samples {
		assert.NotEqual(t, "spoofed-id", sample.RequestID)
		assert.Equal(t, "organization", sample.OrganizationID)
		assert.Equal(t, "selected-worker-id", sample.WorkerRequestID)
		for _, stage := range []gateway.LatencyStage{gateway.LatencySetup, gateway.LatencyAuth, gateway.LatencyBodyRead, gateway.LatencyValidate, gateway.LatencyAdmission, gateway.LatencyBinding, gateway.LatencySigning, gateway.LatencyIAM, gateway.LatencyPrepare, gateway.LatencyConnection, gateway.LatencyWrite, gateway.LatencyDispatch} {
			assert.Contains(t, sample.Milliseconds, stage)
		}
		assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyResponseHeaders], 35.0)
		assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyResponseCopy], 55.0)
		assert.Greater(t, sample.Milliseconds[gateway.LatencyFullResponse], sample.Milliseconds[gateway.LatencyDispatch]+90)
	}
	assert.Contains(t, samples[0].Milliseconds, gateway.LatencyTLS)
	assert.NotContains(t, samples[1].Milliseconds, gateway.LatencyTCP)
	assert.NotContains(t, samples[1].Milliseconds, gateway.LatencyTLS)
}

type delayedGatewayBody struct {
	io.Reader
	delayed bool
}

func (body *delayedGatewayBody) Read(p []byte) (int, error) {
	if !body.delayed {
		time.Sleep(30 * time.Millisecond)
		body.delayed = true
	}
	return body.Reader.Read(p)
}

func TestGatewayLatencyIncludesIncomingBodyWait(t *testing.T) {
	worker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, `{}`, string(body))
		_, _ = io.WriteString(w, "ok")
	}))
	defer worker.Close()
	forwarder, _, _ := gatewayFixture(t, worker, nil, nil)
	var sample gateway.LatencySample
	forwarder.SetLatencyObserver(func(observed gateway.LatencySample) { sample = observed })
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`))
	r.Body = io.NopCloser(&delayedGatewayBody{Reader: strings.NewReader(`{}`)})
	forwarder.ServeHTTP(httptest.NewRecorder(), r)
	assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyBodyRead], 25.0)
	assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyDispatch], 25.0)
}

type delayedGatewayConnection struct {
	net.Conn
	wrote atomic.Bool
}

func (connection *delayedGatewayConnection) Write(p []byte) (int, error) {
	if !connection.wrote.Swap(true) {
		time.Sleep(30 * time.Millisecond)
	}
	return connection.Conn.Write(p)
}

func TestGatewayLatencyIncludesConnectionAndRequestWrite(t *testing.T) {
	payload := `{"padding":"` + strings.Repeat("x", 8192) + `"}`
	worker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		assert.Equal(t, payload, string(body))
		_, _ = io.WriteString(w, "ok")
	}))
	defer worker.Close()
	_, admissions, signer := gatewayFixture(t, worker, nil, nil)
	transport := worker.Client().Transport.(*http.Transport).Clone()
	transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		time.Sleep(40 * time.Millisecond)
		connection, err := (&tls.Dialer{Config: transport.TLSClientConfig}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &delayedGatewayConnection{Conn: connection}, nil
	}
	t.Cleanup(transport.CloseIdleConnections)
	forwarder, err := gateway.NewHandler(credentialVerifier{}, admissions, bindingStore{binding: gatewayBinding(worker.URL)}, signer, revisionAuthorizer{}, transport)
	require.NoError(t, err)
	var sample gateway.LatencySample
	forwarder.SetLatencyObserver(func(observed gateway.LatencySample) { sample = observed })
	w := httptest.NewRecorder()
	forwarder.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload)))
	require.Equal(t, http.StatusOK, w.Code)
	assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyConnection], 35.0)
	assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyWrite], 25.0)
	assert.GreaterOrEqual(t, sample.Milliseconds[gateway.LatencyDispatch], 60.0)
}

type failedGatewayTransport struct{ wrote bool }

func (transport failedGatewayTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	trace := httptrace.ContextClientTrace(request.Context())
	trace.GetConn("worker")
	if transport.wrote {
		trace.GotConn(httptrace.GotConnInfo{})
		trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("synthetic write failure")})
	}
	return nil, errors.New("synthetic transport failure")
}

func TestGatewayLatencyFailedConnectionAndWriteHaveNoDispatchTotal(t *testing.T) {
	worker := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") }))
	defer worker.Close()
	for _, wrote := range []bool{false, true} {
		_, admissions, signer := gatewayFixture(t, worker, nil, nil)
		forwarder, err := gateway.NewHandler(credentialVerifier{}, admissions, bindingStore{binding: gatewayBinding(worker.URL)}, signer, revisionAuthorizer{}, failedGatewayTransport{wrote: wrote})
		require.NoError(t, err)
		var sample gateway.LatencySample
		forwarder.SetLatencyObserver(func(observed gateway.LatencySample) { sample = observed })
		w := httptest.NewRecorder()
		forwarder.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
		require.Equal(t, http.StatusServiceUnavailable, w.Code)
		assert.Contains(t, sample.Milliseconds, gateway.LatencyConnection)
		_, hasWrite := sample.Milliseconds[gateway.LatencyWrite]
		assert.Equal(t, wrote, hasWrite)
		assert.NotContains(t, sample.Milliseconds, gateway.LatencyDispatch)
		assert.NotContains(t, sample.Milliseconds, gateway.LatencyFullResponse)
	}
}

func TestGatewayLatencyTruncatedResponseHasNoCompletedTTLB(t *testing.T) {
	worker := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, "partial")
	}))
	defer worker.Close()
	forwarder, _, _ := gatewayFixture(t, worker, nil, nil)
	var sample gateway.LatencySample
	forwarder.SetLatencyObserver(func(observed gateway.LatencySample) { sample = observed })
	forwarder.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
	assert.Contains(t, sample.Milliseconds, gateway.LatencyDispatch)
	assert.Contains(t, sample.Milliseconds, gateway.LatencyResponseCopy)
	assert.NotContains(t, sample.Milliseconds, gateway.LatencyFullResponse)
}

func TestGatewayLatencySurvivesCancellationBeforeWorkerHeaders(t *testing.T) {
	received := make(chan struct{})
	worker := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(received)
		<-r.Context().Done()
	}))
	defer worker.Close()
	forwarder, _, _ := gatewayFixture(t, worker, nil, nil)
	observed := make(chan gateway.LatencySample, 1)
	forwarder.SetLatencyObserver(func(sample gateway.LatencySample) { observed <- sample })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go forwarder.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)).WithContext(ctx))
	select {
	case <-received:
	case <-ctx.Done():
		t.Fatal("worker did not receive request")
	}
	cancel()
	select {
	case sample := <-observed:
		assert.Contains(t, sample.Milliseconds, gateway.LatencyDispatch)
		assert.Contains(t, sample.Milliseconds, gateway.LatencyWrite)
		assert.NotContains(t, sample.Milliseconds, gateway.LatencyFullResponse)
		assert.Empty(t, sample.WorkerRequestID)
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not finalize telemetry")
	}
}

type failedIAM struct{}

func (failedIAM) IdentityToken(context.Context, string) (string, error) {
	return "", errors.New("synthetic IAM failure")
}

func TestGatewayLatencyPartialAdmissionAndIAM(t *testing.T) {
	worker := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected dispatch") }))
	defer worker.Close()
	for _, test := range []struct {
		name         string
		admissionErr error
		iamFailure   bool
	}{
		{name: "admission", admissionErr: errors.New("synthetic admission failure")},
		{name: "IAM", iamFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			forwarder, admissions, signer := gatewayFixture(t, worker, nil, test.admissionErr)
			if test.iamFailure {
				var err error
				forwarder, err = gateway.NewHandler(credentialVerifier{}, admissions, bindingStore{binding: gatewayBinding(worker.URL)}, signer, failedIAM{}, worker.Client().Transport)
				require.NoError(t, err)
			}
			var sample gateway.LatencySample
			forwarder.SetLatencyObserver(func(observed gateway.LatencySample) { sample = observed })
			w := httptest.NewRecorder()
			forwarder.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
			assert.Equal(t, http.StatusServiceUnavailable, w.Code)
			assert.Contains(t, sample.Milliseconds, gateway.LatencyAdmission)
			_, iam := sample.Milliseconds[gateway.LatencyIAM]
			assert.Equal(t, test.iamFailure, iam)
			assert.NotContains(t, sample.Milliseconds, gateway.LatencyDispatch)
			assert.NotContains(t, sample.Milliseconds, gateway.LatencyFullResponse)
		})
	}
	forwarder, _, _ := gatewayFixture(t, worker, nil, nil)
	var observed int
	forwarder.SetLatencyObserver(func(gateway.LatencySample) { observed++ })
	forwarder.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[{"role":"user","content":"/beta"}]}`)))
	forwarder.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/router/models?scope=catalog", nil))
	assert.Zero(t, observed)
	forwarder, _, _ = gatewayFixture(t, worker, errors.New("synthetic credential rejection"), nil)
	forwarder.SetLatencyObserver(func(gateway.LatencySample) { t.Error("unverified organization must not emit telemetry") })
	forwarder.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))
}
