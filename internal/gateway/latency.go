package gateway

import (
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/google/uuid"
)

// LatencyStage identifies a measured gateway operation, in fractional milliseconds.
type LatencyStage string

// Gateway stages include connection details nested within connection acquisition.
const (
	LatencySetup           LatencyStage = "gateway_setup"
	LatencyAuth            LatencyStage = "gateway_auth"
	LatencyBodyRead        LatencyStage = "gateway_body_read"
	LatencyValidate        LatencyStage = "gateway_validate"
	LatencyAdmission       LatencyStage = "gateway_admission"
	LatencyBinding         LatencyStage = "gateway_binding"
	LatencySigning         LatencyStage = "gateway_signing"
	LatencyIAM             LatencyStage = "gateway_iam"
	LatencyPrepare         LatencyStage = "gateway_prepare"
	LatencyConnection      LatencyStage = "gateway_connection"
	LatencyWrite           LatencyStage = "gateway_write"
	LatencyDispatch        LatencyStage = "gateway_dispatch"
	LatencyDNS             LatencyStage = "gateway_dns"
	LatencyTCP             LatencyStage = "gateway_tcp"
	LatencyTLS             LatencyStage = "gateway_tls"
	LatencyResponseHeaders LatencyStage = "gateway_response_headers"
	LatencyResponseCopy    LatencyStage = "gateway_response_copy"
	LatencyFullResponse    LatencyStage = "full_e2e"
)

// LatencySample belongs to the verified organization. RequestID is telemetry-only;
// WorkerRequestID is the optional ID observed on the selected worker's response.
// Absent stages were not measured; a present zero is a measurement.
type LatencySample struct {
	RequestID, WorkerRequestID, OrganizationID string
	Start, End                                 time.Time
	StatusCode                                 int
	Milliseconds                               map[LatencyStage]float64
}

// SetLatencyObserver wires best-effort telemetry before serving requests.
func (h *Handler) SetLatencyObserver(observe func(LatencySample)) {
	h.observeLatency = observe
}

type latencyRecorder struct {
	mu         sync.Mutex
	clock      func() time.Time
	sample     LatencySample
	stage      LatencyStage
	boundary   time.Time
	wrote      time.Time
	headers    bool
	frozen     bool
	operations map[string][]time.Time
}

func newLatencyRecorder(clock func() time.Time) *latencyRecorder {
	start := clock()
	return &latencyRecorder{clock: clock, stage: LatencySetup, boundary: start,
		sample:     LatencySample{RequestID: uuid.NewString(), Start: start, Milliseconds: make(map[LatencyStage]float64)},
		operations: make(map[string][]time.Time)}
}

func (l *latencyRecorder) begin(stage LatencyStage) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.frozen {
		l.transition(stage, l.clock())
	}
}

func (l *latencyRecorder) transition(stage LatencyStage, now time.Time) {
	if l.stage != "" {
		l.sample.Milliseconds[l.stage] += float64(now.Sub(l.boundary)) / float64(time.Millisecond)
	}
	l.stage, l.boundary = stage, now
}

func (l *latencyRecorder) networkStart(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.frozen {
		l.operations[key] = append(l.operations[key], l.clock())
	}
}

func (l *latencyRecorder) networkDone(key string, stage LatencyStage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	starts := l.operations[key]
	if l.frozen || len(starts) == 0 {
		return
	}
	l.sample.Milliseconds[stage] += float64(l.clock().Sub(starts[0])) / float64(time.Millisecond)
	l.operations[key] = starts[1:]
}

func (l *latencyRecorder) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) { l.connectionStage(LatencyConnection) },
		GotConn: func(httptrace.GotConnInfo) { l.connectionStage(LatencyWrite) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.frozen || !l.wrote.IsZero() || info.Err != nil {
				return
			}
			now := l.clock()
			l.wrote = now
			l.sample.Milliseconds[LatencyDispatch] = float64(now.Sub(l.sample.Start)) / float64(time.Millisecond)
			if !l.headers {
				l.transition(LatencyResponseHeaders, now)
			}
		},
		DNSStart:          func(httptrace.DNSStartInfo) { l.networkStart("dns") },
		DNSDone:           func(httptrace.DNSDoneInfo) { l.networkDone("dns", LatencyDNS) },
		ConnectStart:      func(network, address string) { l.networkStart(network + ":" + address) },
		ConnectDone:       func(network, address string, _ error) { l.networkDone(network+":"+address, LatencyTCP) },
		TLSHandshakeStart: func() { l.networkStart("tls") },
		TLSHandshakeDone:  func(_ tls.ConnectionState, _ error) { l.networkDone("tls", LatencyTLS) },
	}
}

func (l *latencyRecorder) connectionStage(stage LatencyStage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.frozen && l.wrote.IsZero() && !l.headers {
		l.transition(stage, l.clock())
	}
}

func (l *latencyRecorder) response(response *http.Response) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.headers = true
	l.sample.WorkerRequestID = response.Header.Get("X-Request-Id")
	l.sample.StatusCode = response.StatusCode
	l.transition(LatencyResponseCopy, l.clock())
}

func (l *latencyRecorder) finish(completed bool) LatencySample {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sample.End = l.clock()
	l.transition("", l.sample.End)
	if completed {
		l.sample.Milliseconds[LatencyFullResponse] = float64(l.sample.End.Sub(l.sample.Start)) / float64(time.Millisecond)
	}
	l.frozen = true
	return l.sample
}

type observedResponseBody struct {
	io.ReadCloser
	eof bool
}

func (b *observedResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		b.eof = true
	}
	return n, err
}

func latencySurface(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/chat/completions", "/v1/responses":
		return true
	default:
		return singlePathParameter(r.URL.Path, "/v1beta/models/", ":generateContent") || singlePathParameter(r.URL.Path, "/v1beta/models/", ":streamGenerateContent")
	}
}
