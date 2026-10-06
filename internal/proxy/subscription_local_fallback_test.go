package proxy_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/subscriptions"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fallbackClaudeToken  = "sk-ant-oat01-local-fallback-test"
	fallbackCodexToken   = "eyJhbGciOiJIUzI1NiJ9.local-fallback-codex.signature"
	fallbackCodexAccount = "acct_local_fallback"
	fallbackCodexModel   = "gpt-5.6-sol"
)

var (
	claudeLimit429 = &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your account's rate limit."}}`),
	}
	codexLimit429 = &providers.UpstreamErrorResponse{
		Status: http.StatusTooManyRequests,
		Body:   []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`),
	}
	// A dispatch with no credential reaches the provider without a key.
	missingKey401 = &providers.UpstreamErrorResponse{
		Status: http.StatusUnauthorized,
		Body:   []byte(`{"error":{"type":"authentication_error","message":"missing api key"}}`),
	}
)

// subscriptionUpstream fails every dispatch carrying a subscription OAuth
// credential with subErr and answers the rest with paidErr or okBody.
type subscriptionUpstream struct {
	subErr     error
	paidErr    error
	preErrBody string
	okBody     func(w http.ResponseWriter)

	subDispatches  int
	paidDispatches int
}

func (*subscriptionUpstream) IncludedOnlySubscriptions() bool { return true }

func (u *subscriptionUpstream) Proxy(ctx context.Context, _ router.Decision, _ providers.PreparedRequest, w http.ResponseWriter, _ *http.Request) error {
	if creds := proxy.CredentialsFromContext(ctx); creds != nil && creds.OAuth {
		u.subDispatches++
		if u.preErrBody != "" {
			_, _ = io.WriteString(w, u.preErrBody)
		}
		if u.subErr == nil {
			u.okBody(w)
		}
		return u.subErr
	}
	u.paidDispatches++
	if u.paidErr != nil {
		return u.paidErr
	}
	u.okBody(w)
	return nil
}

func (*subscriptionUpstream) Passthrough(context.Context, providers.PreparedRequest, http.ResponseWriter, *http.Request) error {
	return providers.ErrNotImplemented
}

func anthropicOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"msg_paid","type":"message","role":"assistant","content":[{"type":"text","text":"paid"}],"stop_reason":"end_turn"}`)
}

func localChatOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"id":"chatcmpl_local","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"served locally"},"finish_reason":"stop"}]}`)
}

type subscriptionFallbackFixture struct {
	svc      *proxy.Service
	store    *fakePinStore
	upstream *subscriptionUpstream
	local    *fakeProvider
	provider string
	model    string
}

// newSubscriptionFallbackFixture wires one subscription provider whose scorer
// pick is model, plus a local model. paidKey adds a deployment key for the
// subscription provider; enabled installs the subscription fallback.
func newSubscriptionFallbackFixture(t *testing.T, id, subProvider, model string, paidKey, enabled bool, mutate func(*catalog.Model)) subscriptionFallbackFixture {
	t.Helper()
	localProvider := providers.LocalProviderName(id)
	localModel := catalog.Model{
		ID:            id,
		Tier:          catalog.TierMid,
		ContextWindow: 32_000,
		ImageInput:    catalog.ImageInputUnsupported,
		Providers:     []catalog.ProviderBinding{{Provider: localProvider, UpstreamID: "upstream-" + id}},
	}
	if mutate != nil {
		mutate(&localModel)
	}
	registerTestLocalModel(t, localModel)

	f := subscriptionFallbackFixture{
		upstream: &subscriptionUpstream{okBody: anthropicOK},
		local:    &fakeProvider{proxyResponse: localChatOK},
		store:    newFakePinStore(),
		provider: localProvider,
		model:    id,
	}
	if !paidKey {
		f.upstream.paidErr = missingKey401
	}
	keyed := map[string]struct{}{localProvider: {}}
	if paidKey {
		keyed[subProvider] = struct{}{}
	}
	f.svc = proxy.NewService(
		&fakeRouter{decision: router.Decision{Provider: subProvider, Model: model, Reason: "cluster"}},
		map[string]providers.Client{subProvider: f.upstream, localProvider: f.local},
		nil, false, nil, f.store, false,
		providers.ProviderAnthropic, "claude-haiku-4-5", nil,
	).WithDeploymentKeyedProviders(keyed)
	if enabled {
		f.svc.WithSubscriptionLocalFallback(proxy.SubscriptionLocalFallback{Provider: localProvider, Model: id})
	}
	return f
}

func claudeSubscriptionCtx() context.Context {
	return context.WithValue(authedCtx(uuid.New().String()), proxy.AnthropicSubscriptionContextKey{}, fallbackClaudeToken)
}

func (f subscriptionFallbackFixture) messages(t *testing.T, ctx context.Context, body string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	return rec, f.svc.ProxyMessages(ctx, []byte(body), rec, req)
}

func TestSubscriptionLocalFallback_ClaudeRateLimitServedLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-claude", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	f.upstream.subErr = claudeLimit429
	var logs bytes.Buffer
	ctx := observability.WithLogger(claudeSubscriptionCtx(), slog.New(slog.NewJSONHandler(&logs, nil)))

	rec, err := f.messages(t, ctx, pinTestBody)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, f.upstream.subDispatches, "the subscription is tried first")
	require.Len(t, f.local.proxyBodies, 1, "the local model serves the refused turn")
	assert.Nil(t, f.local.proxyCreds[0], "the subscription credential never reaches the local server")
	assert.Contains(t, rec.Body.String(), "served locally")
	assert.Equal(t, f.model, rec.Header().Get(proxy.HeaderRouterModel))
	assert.Equal(t, f.provider, rec.Header().Get(proxy.HeaderRouterProvider))

	assert.Contains(t, logs.String(), `"Subscription local fallback serving turn"`)
	line := completionLine(t, &logs)
	assert.Equal(t, "✦ **Weave Router** → "+f.model+" (local) · local fallback after claude-opus-4-7 subscription limit\n\n", line["routing_marker"])
	assert.Equal(t, f.model, line["decision_model"])
	assert.Equal(t, f.provider, line["decision_provider"])
	assert.Equal(t, "claude-opus-4-7", line["substituted_from_model"])
	assert.Equal(t, providers.ProviderAnthropic, line["substituted_from_provider"])
	assert.Equal(t, true, line["subscription_local_fallback"])
}

func TestSubscriptionLocalFallback_ClaudeObservedExhaustionServedLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-observed", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	now := time.Now()
	observer := usage.NewObserver([]byte("salt"), time.Hour, func() time.Time { return now })
	observer.Record(observer.Key([]byte(fallbackClaudeToken)), usage.Snapshot{Secondary: usage.Window{UsedPercent: 1, WindowMinutes: 10080}})
	f.svc.WithUsageObserver(observer)

	rec, err := f.messages(t, claudeSubscriptionCtx(), pinTestBody)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, f.upstream.subDispatches, "a plan already read spent is not dispatched again")
	assert.Zero(t, f.upstream.paidDispatches, "with no paid key the prompt never reaches the vendor")
	assert.Len(t, f.local.proxyBodies, 1)
	assert.Contains(t, rec.Body.String(), "served locally")
}

func TestSubscriptionLocalFallback_CodexObservedExhaustionServedLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-codex-observed", providers.ProviderOpenAI, fallbackCodexModel, false, true, nil)
	now := time.Now()
	observer := usage.NewObserver([]byte("salt"), time.Hour, func() time.Time { return now })
	observer.Record(observer.Key([]byte(fallbackCodexToken)), usage.Snapshot{Secondary: usage.Window{UsedPercent: 1, WindowMinutes: 10080}})
	f.svc.WithUsageObserver(observer)
	body := []byte(`{"model":"` + fallbackCodexModel + `","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	require.NoError(t, f.svc.ProxyOpenAIResponses(codexSubscriptionCtx(io.Discard), body, rec, req))

	assert.Zero(t, f.upstream.subDispatches+f.upstream.paidDispatches, "with no paid key the prompt never reaches the vendor")
	assert.Len(t, f.local.proxyBodies, 1)
	assert.Contains(t, rec.Body.String(), "served locally")
}

// A Claude turn on the Chat Completions ingress keeps the spent token attached;
// the local model still answers before the vendor sees the prompt.
func TestSubscriptionLocalFallback_ClaudeObservedExhaustionOnChatIngressSkipsVendor(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-chat-observed", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	now := time.Now()
	observer := usage.NewObserver([]byte("salt"), time.Hour, func() time.Time { return now })
	observer.Record(observer.Key([]byte(fallbackClaudeToken)), usage.Snapshot{Secondary: usage.Window{UsedPercent: 1, WindowMinutes: 10080}})
	f.svc.WithUsageObserver(observer)
	body := []byte(`{"model":"claude-opus-4-7","messages":[{"role":"user","content":"fix the build"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(""))

	require.NoError(t, f.svc.ProxyOpenAIChatCompletion(claudeSubscriptionCtx(), body, rec, req))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Zero(t, f.upstream.subDispatches+f.upstream.paidDispatches, "the vendor never receives the prompt")
	assert.Len(t, f.local.proxyBodies, 1)
	assert.Contains(t, rec.Body.String(), "served locally")
}

type oneSeatLeaser struct{ leased int }

func (l *oneSeatLeaser) Lease(context.Context, auth.SubscriptionOwner, subscriptions.Provider, string) (subscriptions.Lease, bool, error) {
	if l.leased > 0 {
		return subscriptions.Lease{}, true, subscriptions.ErrNoAvailableAccount
	}
	l.leased++
	return subscriptions.Lease{AccountID: "seat-1", AccessToken: "sk-ant-oat01-managed-seat"}, true, nil
}

func (*oneSeatLeaser) Cooldown(context.Context, auth.SubscriptionOwner, subscriptions.Provider, string, time.Time) error {
	return nil
}

func (*oneSeatLeaser) Disable(context.Context, auth.SubscriptionOwner, subscriptions.Provider, string) error {
	return nil
}

// The caller's own plan is spent, but an enrolled managed seat can still serve
// the selection, so the vendor is tried before the local model.
func TestSubscriptionLocalFallback_ObservedExhaustionKeepsManagedSeat(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-managed-seat", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	now := time.Now()
	observer := usage.NewObserver([]byte("salt"), time.Hour, func() time.Time { return now })
	observer.Record(observer.Key([]byte(fallbackClaudeToken)), usage.Snapshot{Secondary: usage.Window{UsedPercent: 1, WindowMinutes: 10080}})
	f.svc.WithUsageObserver(observer).WithManagedSubscriptions(&oneSeatLeaser{})
	ctx := context.WithValue(claudeSubscriptionCtx(), proxy.ManagedSubscriptionProvidersContextKey{}, map[auth.SubscriptionProvider]struct{}{auth.SubscriptionProviderClaude: {}})
	ctx = proxy.WithManagedSubscriptionUsage(ctx)

	rec, _ := f.messages(t, ctx, pinTestBody)

	assert.Equal(t, 1, f.upstream.subDispatches, "the managed seat serves the turn")
	assert.Empty(t, f.local.proxyBodies)
	assert.Contains(t, rec.Body.String(), `"paid"`)
}

func TestSubscriptionLocalFallback_PaidKeyKeepsPrecedence(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-paid", providers.ProviderAnthropic, "claude-opus-4-7", true, true, nil)
	f.upstream.subErr = claudeLimit429

	rec, err := f.messages(t, claudeSubscriptionCtx(), pinTestBody)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, 1, f.upstream.paidDispatches, "the deployment key retries the requested model first")
	assert.Empty(t, f.local.proxyBodies)
	assert.Contains(t, rec.Body.String(), `"paid"`)
}

func TestSubscriptionLocalFallback_PaidKeyFailureFallsBackLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-paid-fail", providers.ProviderAnthropic, "claude-opus-4-7", true, true, nil)
	f.upstream.subErr = claudeLimit429
	f.upstream.paidErr = &providers.UpstreamErrorResponse{Status: http.StatusTooManyRequests, Body: []byte(`{"error":{"type":"rate_limit_error"}}`)}

	rec, err := f.messages(t, claudeSubscriptionCtx(), pinTestBody)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, f.upstream.paidDispatches)
	assert.Len(t, f.local.proxyBodies, 1)
}

func TestSubscriptionLocalFallback_ErrorSurfaces(t *testing.T) {
	cases := map[string]struct {
		subErr     error
		preErrBody string
		enabled    bool
		ctx        func(f subscriptionFallbackFixture) context.Context
		headers    http.Header
		mutate     func(*catalog.Model)
		wantStatus int
	}{
		"authentication rejection": {
			subErr:     missingKey401,
			enabled:    true,
			wantStatus: http.StatusUnauthorized,
		},
		"invalid request": {
			subErr:     &providers.UpstreamErrorResponse{Status: http.StatusBadRequest, Body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`)},
			enabled:    true,
			wantStatus: http.StatusBadRequest,
		},
		"fallback disabled": {
			subErr:     claudeLimit429,
			wantStatus: http.StatusTooManyRequests,
		},
		"installation excluded the local model": {
			subErr:  claudeLimit429,
			enabled: true,
			ctx: func(f subscriptionFallbackFixture) context.Context {
				return context.WithValue(claudeSubscriptionCtx(), proxy.InstallationExcludedModelsContextKey{}, []string{f.model})
			},
			wantStatus: http.StatusTooManyRequests,
		},
		"history beyond the local context window": {
			subErr:     claudeLimit429,
			enabled:    true,
			mutate:     func(m *catalog.Model) { m.ContextWindow = 1 },
			wantStatus: http.StatusTooManyRequests,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSubscriptionFallbackFixture(t, "test-sub-fb-surface", providers.ProviderAnthropic, "claude-opus-4-7", false, tc.enabled, tc.mutate)
			f.upstream.subErr = tc.subErr
			ctx := claudeSubscriptionCtx()
			if tc.ctx != nil {
				ctx = tc.ctx(f)
			}

			rec, err := f.messages(t, ctx, pinTestBody)

			require.Error(t, err)
			assert.Empty(t, f.local.proxyBodies, "the local model never serves this turn")
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}

func TestSubscriptionLocalFallback_ForcedModelIsNotFallenBack(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-forced", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	f.upstream.subErr = claudeLimit429
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	req.Header.Set("X-Weave-Force-Model", "claude-opus-4-7")

	err := f.svc.ProxyMessages(claudeSubscriptionCtx(), []byte(pinTestBody), rec, req)

	require.Error(t, err)
	assert.Empty(t, f.local.proxyBodies)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestSubscriptionLocalFallback_CommittedStreamIsNotRetried(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-committed", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	f.upstream.subErr = &providers.UpstreamStatusError{Status: http.StatusTooManyRequests}
	f.upstream.preErrBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n"
	body := `{"model":"claude-opus-4-7","stream":true,"system":"sys","messages":[{"role":"user","content":"original prompt"}]}`

	_, err := f.messages(t, claudeSubscriptionCtx(), body)

	require.Error(t, err)
	assert.Empty(t, f.local.proxyBodies, "bytes already reached the client, so the turn is never re-served")
}

func TestSubscriptionLocalFallback_CodexRateLimitOnResponsesServedLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-codex", providers.ProviderOpenAI, fallbackCodexModel, false, true, nil)
	f.upstream.subErr = codexLimit429
	var logs bytes.Buffer
	ctx := context.WithValue(authedCtx(uuid.New().String()), proxy.OpenAISubscriptionContextKey{}, fallbackCodexToken)
	ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, fallbackCodexAccount)
	ctx = context.WithValue(ctx, proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{ClientApp: proxy.ClientAppCodex})
	ctx = observability.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))
	body := []byte(`{"model":"` + fallbackCodexModel + `","stream":false,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	require.NoError(t, f.svc.ProxyOpenAIResponses(ctx, body, rec, req))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Positive(t, f.upstream.subDispatches, "the Codex subscription is tried first")
	require.Len(t, f.local.proxyBodies, 1, "the local model serves the refused turn")
	assert.Nil(t, f.local.proxyCreds[0])
	assert.Equal(t, providers.EndpointChatCompletions, f.local.proxyEndpoints[0])
	assert.Contains(t, rec.Body.String(), "served locally")
	assert.Contains(t, rec.Body.String(), `"status":"completed"`)
	assert.Contains(t, logs.String(), `"Subscription local fallback serving turn"`)
	assert.Contains(t, logs.String(), `"original_model":"`+fallbackCodexModel+`"`)
}

// The fallback never displaces the subscription: the session keeps the
// original selection, so the next turn goes back to the subscription.
func TestSubscriptionLocalFallback_NextTurnReturnsToSubscription(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-next", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	f.store.persistUpserts = true
	f.upstream.subErr = claudeLimit429
	f.local.proxyResponse = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_local","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"served locally"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2000,"completion_tokens":5,"total_tokens":2005}}`)
	}
	ctx := claudeSubscriptionCtx()

	_, err := f.messages(t, ctx, pinTestBody)
	require.NoError(t, err)
	require.Len(t, f.local.proxyBodies, 1)

	f.upstream.subErr = nil
	rec, err := f.messages(t, ctx, pinTestBody)

	require.NoError(t, err)
	assert.Contains(t, rec.Body.String(), `"paid"`, "the recovered subscription serves the next turn")
	assert.Equal(t, "claude-opus-4-7", rec.Header().Get(proxy.HeaderRouterModel))
	assert.Len(t, f.local.proxyBodies, 1, "the local model is not reused once the subscription recovers")
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	require.NotEmpty(t, f.store.usages)
	assert.Equal(t, "claude-opus-4-7", f.store.usages[0].ServedModel, "the fallback turn records the original selection")
}

func localChatStreamOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl_local\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"served locally\"},\"finish_reason\":null}]}\n\n"+
		"data: {\"id\":\"chatcmpl_local\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n")
}

func codexSubscriptionCtx(logs io.Writer) context.Context {
	ctx := context.WithValue(authedCtx(uuid.New().String()), proxy.OpenAISubscriptionContextKey{}, fallbackCodexToken)
	ctx = context.WithValue(ctx, proxy.OpenAIAccountIDContextKey{}, fallbackCodexAccount)
	ctx = context.WithValue(ctx, proxy.ClientIdentityContextKey{}, proxy.ClientIdentity{ClientApp: proxy.ClientAppCodex})
	return observability.WithLogger(ctx, slog.New(slog.NewJSONHandler(logs, nil)))
}

func fallbackBadge(f subscriptionFallbackFixture) string {
	return "→ " + f.model + " (local) · local fallback after"
}

// streamedCount counts needle in the text deltas of an SSE body, where each
// rendered badge appears once; done and completed events repeat the text.
func streamedCount(body, needle string) int {
	n := 0
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && (strings.Contains(line, `"type":"response.output_text.delta"`) || strings.Contains(line, `"type":"content_block_delta"`)) {
			n += strings.Count(line, needle)
		}
	}
	return n
}

func TestSubscriptionLocalFallback_ClaudeRateLimitStreamServedLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-claude-stream", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	f.upstream.subErr = claudeLimit429
	f.local.proxyResponse = localChatStreamOK
	body := `{"model":"claude-opus-4-7","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"fix the build"}]}`

	rec, err := f.messages(t, claudeSubscriptionCtx(), body)

	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, f.local.proxyBodies, 1, "the local model serves the refused turn")
	out := rec.Body.String()
	assert.Equal(t, 1, strings.Count(out, "event: message_start"))
	assert.NotContains(t, out, "event: error")
	assert.Contains(t, out, "served locally")
	assert.Equal(t, 1, streamedCount(out, fallbackBadge(f)), "the fallback badge renders once")
	assert.NotContains(t, out, "→ claude-opus-4-7", "the refused selection's badge never reaches the client")
}

func TestSubscriptionLocalFallback_CodexRateLimitOnResponsesStreamServedLocally(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-codex-stream", providers.ProviderOpenAI, fallbackCodexModel, false, true, nil)
	f.upstream.subErr = codexLimit429
	f.local.proxyResponse = localChatStreamOK
	body := []byte(`{"model":"` + fallbackCodexModel + `","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	require.NoError(t, f.svc.ProxyOpenAIResponses(codexSubscriptionCtx(io.Discard), body, rec, req))

	assert.Positive(t, f.upstream.subDispatches, "the Codex subscription is tried first")
	require.Len(t, f.local.proxyBodies, 1, "the local model serves the refused turn")
	out := rec.Body.String()
	assert.Equal(t, 1, strings.Count(out, "event: response.created"))
	assert.Equal(t, 1, strings.Count(out, "event: response.output_item.added"), "badge and answer share one assistant item")
	assert.NotContains(t, out, "event: response.failed")
	assert.Contains(t, out, "served locally")
	assert.Contains(t, out, "event: response.completed")
	assert.Equal(t, 1, streamedCount(out, fallbackBadge(f)), "the fallback badge renders once")
	assert.NotContains(t, out, "best pick", "the refused selection's badge never reaches the client")
}

// Planning the fallback defers the Responses badge to the first output; a turn
// the subscription serves still shows its own badge exactly once.
func TestSubscriptionLocalFallback_CodexStreamServedBySubscriptionKeepsOneBadge(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-codex-ok", providers.ProviderOpenAI, fallbackCodexModel, false, true, nil)
	f.upstream.okBody = func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		item := `{"id":"msg_sub","type":"message","status":"in_progress","role":"assistant","content":[]}`
		done := `{"id":"msg_sub","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"from subscription","annotations":[]}]}`
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_sub\",\"status\":\"in_progress\",\"output\":[]}}\n\n"+
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":"+item+"}\n\n"+
			"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"sequence_number\":2,\"item_id\":\"msg_sub\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\",\"annotations\":[]}}\n\n"+
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_sub\",\"output_index\":0,\"content_index\":0,\"delta\":\"from subscription\"}\n\n"+
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":4,\"output_index\":0,\"item\":"+done+"}\n\n"+
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":5,\"response\":{\"id\":\"resp_sub\",\"status\":\"completed\",\"output\":["+done+"]}}\n\n")
	}
	body := []byte(`{"model":"` + fallbackCodexModel + `","stream":true,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the build"}]}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(""))

	require.NoError(t, f.svc.ProxyOpenAIResponses(codexSubscriptionCtx(io.Discard), body, rec, req))

	assert.Empty(t, f.local.proxyBodies)
	out := rec.Body.String()
	assert.Contains(t, out, "from subscription")
	assert.Equal(t, 1, streamedCount(out, "→ "+fallbackCodexModel+" · best pick"), "the subscription's badge renders once")
	assert.NotContains(t, out, "local fallback after")
}

// A paid retry's own request rejection is the turn's real error; the earlier
// subscription limit does not license serving it locally.
func TestSubscriptionLocalFallback_PaidRetryRejectionSurfaces(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-paid-400", providers.ProviderAnthropic, "claude-opus-4-7", true, true, nil)
	f.upstream.subErr = claudeLimit429
	f.upstream.paidErr = &providers.UpstreamErrorResponse{Status: http.StatusBadRequest, Body: []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"tools.0: unknown field"}}`)}

	rec, err := f.messages(t, claudeSubscriptionCtx(), pinTestBody)

	require.Error(t, err)
	assert.Equal(t, 1, f.upstream.paidDispatches)
	assert.Empty(t, f.local.proxyBodies, "the paid key's 400 is not served locally")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "unknown field")
}

func TestSubscriptionLocalFallback_ObservedExhaustionLocalFailureSurfacesLimit(t *testing.T) {
	f := newSubscriptionFallbackFixture(t, "test-sub-fb-observed-fail", providers.ProviderAnthropic, "claude-opus-4-7", false, true, nil)
	now := time.Now()
	observer := usage.NewObserver([]byte("salt"), time.Hour, func() time.Time { return now })
	observer.Record(observer.Key([]byte(fallbackClaudeToken)), usage.Snapshot{Secondary: usage.Window{UsedPercent: 1, WindowMinutes: 10080}})
	f.svc.WithUsageObserver(observer)
	f.local.proxyErr = &providers.UpstreamErrorResponse{Status: http.StatusBadGateway, Body: []byte(`{"error":{"type":"server_error","message":"local down"}}`)}

	rec, err := f.messages(t, claudeSubscriptionCtx(), pinTestBody)

	require.Error(t, err)
	assert.Zero(t, f.upstream.subDispatches+f.upstream.paidDispatches, "the vendor never receives the prompt")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "the client sees the subscription limit, not the local failure")
	assert.Contains(t, rec.Body.String(), "rate_limit_error")
}
