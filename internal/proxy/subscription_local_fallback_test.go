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

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"

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
	assert.Len(t, f.local.proxyBodies, 1)
	assert.Contains(t, rec.Body.String(), "served locally")
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
