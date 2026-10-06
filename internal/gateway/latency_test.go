package gateway

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatencyMilestonesAndRetries(t *testing.T) {
	now := time.Unix(100, 0)
	l := newLatencyRecorder(func() time.Time { return now })
	advance := func(stage LatencyStage, d time.Duration) { now = now.Add(d); l.begin(stage) }
	advance(LatencyAuth, 250*time.Microsecond)
	advance(LatencyBodyRead, 2*time.Millisecond)
	advance(LatencyValidate, 3*time.Millisecond)
	advance(LatencyAdmission, time.Millisecond)
	advance(LatencyBinding, 4*time.Millisecond)
	advance(LatencySigning, 500*time.Microsecond)
	advance(LatencyIAM, 125*time.Microsecond)
	advance(LatencyPrepare, 2*time.Millisecond)
	trace := l.trace()
	now = now.Add(time.Millisecond)
	trace.GetConn("worker")
	trace.DNSStart(httptrace.DNSStartInfo{})
	now = now.Add(250 * time.Microsecond)
	trace.DNSDone(httptrace.DNSDoneInfo{})
	trace.ConnectStart("tcp", "worker:443")
	now = now.Add(500 * time.Microsecond)
	trace.ConnectDone("tcp", "worker:443", nil)
	trace.TLSHandshakeStart()
	now = now.Add(750 * time.Microsecond)
	trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
	trace.GotConn(httptrace.GotConnInfo{})
	now = now.Add(time.Millisecond)
	trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("retry")})
	trace.GetConn("worker")
	now = now.Add(500 * time.Microsecond)
	trace.GotConn(httptrace.GotConnInfo{Reused: true})
	now = now.Add(500 * time.Microsecond)
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	now = now.Add(40 * time.Millisecond)
	l.response(&http.Response{StatusCode: 200, Header: http.Header{"X-Request-Id": {"worker-id"}}})
	now = now.Add(60 * time.Millisecond)
	sample := l.finish(true)
	assert.Equal(t, "worker-id", sample.WorkerRequestID)
	assert.Equal(t, 200, sample.StatusCode)
	assert.Equal(t, map[LatencyStage]float64{
		LatencySetup: .25, LatencyAuth: 2, LatencyBodyRead: 3, LatencyValidate: 1,
		LatencyAdmission: 4, LatencyBinding: .5, LatencySigning: .125, LatencyIAM: 2,
		LatencyPrepare: 1, LatencyConnection: 2, LatencyWrite: 1.5,
		LatencyDNS: .25, LatencyTCP: .5, LatencyTLS: .75,
		LatencyDispatch: 17.375, LatencyResponseHeaders: 40, LatencyResponseCopy: 60,
		LatencyFullResponse: 117.375,
	}, sample.Milliseconds)
	// Completed observations cannot be changed by late or duplicate transport hooks.
	now = now.Add(time.Second)
	trace.GetConn("worker")
	trace.GotConn(httptrace.GotConnInfo{})
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	trace.ConnectStart("tcp", "worker:443")
	trace.ConnectDone("tcp", "worker:443", nil)
	assert.Equal(t, 17.375, sample.Milliseconds[LatencyDispatch])
}

func TestLatencyIncompleteAndEarlyResponse(t *testing.T) {
	for _, successfulWrite := range []bool{false, true} {
		now := time.Unix(100, 0)
		l := newLatencyRecorder(func() time.Time { return now })
		l.begin(LatencyPrepare)
		trace := l.trace()
		trace.GetConn("worker")
		trace.GotConn(httptrace.GotConnInfo{Reused: true})
		now = now.Add(2 * time.Millisecond)
		l.response(&http.Response{StatusCode: 413, Header: http.Header{}})
		now = now.Add(time.Millisecond)
		if successfulWrite {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		}
		sample := l.finish(false)
		_, dispatched := sample.Milliseconds[LatencyDispatch]
		assert.Equal(t, successfulWrite, dispatched)
		assert.NotContains(t, sample.Milliseconds, LatencyFullResponse)
		assert.NotContains(t, sample.Milliseconds, LatencyResponseHeaders)
		assert.NotContains(t, sample.Milliseconds, LatencyDNS)
		assert.NotContains(t, sample.Milliseconds, LatencyTLS)
		assert.Contains(t, sample.Milliseconds, LatencyConnection)
		assert.Equal(t, 0.0, sample.Milliseconds[LatencyConnection])
	}
}

func TestLatencyConcurrentHooksFreeze(t *testing.T) {
	l := newLatencyRecorder(time.Now)
	trace := l.trace()
	var hooks sync.WaitGroup
	for range 20 {
		hooks.Go(func() {
			trace.ConnectStart("tcp", "worker")
			trace.ConnectDone("tcp", "worker", nil)
			trace.GetConn("worker")
			trace.GotConn(httptrace.GotConnInfo{})
			trace.WroteRequest(httptrace.WroteRequestInfo{})
		})
	}
	sample := l.finish(false)
	before := make(map[LatencyStage]float64, len(sample.Milliseconds))
	for stage, value := range sample.Milliseconds {
		before[stage] = value
	}
	hooks.Wait()
	trace.GetConn("worker")
	trace.GotConn(httptrace.GotConnInfo{})
	trace.WroteRequest(httptrace.WroteRequestInfo{})
	trace.ConnectStart("tcp", "worker")
	trace.ConnectDone("tcp", "worker", nil)
	assert.Equal(t, before, sample.Milliseconds)
	require.NotEmpty(t, sample.RequestID)
	assert.NotContains(t, sample.Milliseconds, LatencyFullResponse)
}
