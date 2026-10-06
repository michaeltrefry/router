package proxy

import (
	"context"
	"errors"
	"net/http"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
)

// reasonSubscriptionLocalFallback is the decision reason of a turn served by
// the local model after the caller's subscription refused it.
const reasonSubscriptionLocalFallback = "subscription_local_fallback"

// markerReasonSubscriptionLocalFallback prefixes the original model in the
// routing marker of a turn the subscription fallback served.
const markerReasonSubscriptionLocalFallback = "local fallback after"

// SubscriptionLocalFallback serves turns a Claude or Codex subscription
// refused for rate limit or plan exhaustion on one deployment-configured
// self-hosted model.
type SubscriptionLocalFallback struct {
	Provider string
	Model    string
}

// WithSubscriptionLocalFallback installs the subscription fallback; an empty
// provider or model disables it.
func (s *Service) WithSubscriptionLocalFallback(fb SubscriptionLocalFallback) *Service {
	s.subscriptionFallbackProvider, s.subscriptionFallbackModel = "", ""
	if fb.Provider == "" || fb.Model == "" {
		return s
	}
	s.subscriptionFallbackProvider, s.subscriptionFallbackModel = fb.Provider, fb.Model
	return s
}

// subscriptionRefusalNote records the first pre-commit attempt a subscription
// credential refused for its limit. Dispatch can retry that attempt on another
// credential, so the error that finally reaches the ingress may not show it.
type subscriptionRefusalNote struct {
	err error
}

type subscriptionRefusalNoteKey struct{}

func withSubscriptionRefusalNote(ctx context.Context) (context.Context, *subscriptionRefusalNote) {
	note := &subscriptionRefusalNote{}
	return context.WithValue(ctx, subscriptionRefusalNoteKey{}, note), note
}

// noteSubscriptionRefusal records err when credentialCtx dispatched on a
// subscription OAuth credential and err is that subscription's limit refusal.
func noteSubscriptionRefusal(ctx, credentialCtx context.Context, err error) {
	note, _ := ctx.Value(subscriptionRefusalNoteKey{}).(*subscriptionRefusalNote)
	if note == nil || note.err != nil || !subscriptionLimitRefusal(err) {
		return
	}
	if creds := CredentialsFromContext(credentialCtx); creds == nil || !creds.OAuth {
		return
	}
	note.err = err
}

// subscriptionLimitRefusal reports a rate-limit or plan-exhaustion refusal: a
// buffered 429, or a Codex quota rejection body. Committed streams never qualify.
func subscriptionLimitRefusal(err error) bool {
	if providers.IsUpstreamRateLimited(err) {
		return true
	}
	_, quota := codexQuotaExhaustion(err)
	return quota
}

// subscriptionLocalFallback is one turn's fallback plan: the local target, the
// selection it replaces, and how the subscription's refusal is recognized.
type subscriptionLocalFallback struct {
	target   router.Decision
	original router.Decision
	note     *subscriptionRefusalNote
	// exhaustedUnfunded is a subscription the observer already read spent, with
	// no paid key to serve the requested model in its place.
	exhaustedUnfunded bool
	// localFirst skips the vendor: exhaustedUnfunded with no managed seat either.
	localFirst bool
	done       bool
}

// planSubscriptionLocalFallback returns the turn's fallback plan when the
// fallback is configured, the turn is about to dispatch on the caller's
// subscription (or one already read spent with no paid key behind it), the
// selection is neither forced nor already local, and the local model can
// take the request. A nil plan leaves every existing failure path unchanged.
func (s *Service) planSubscriptionLocalFallback(ctx context.Context, res turnLoopResult, req router.Request, decision router.Decision, headers http.Header, note *subscriptionRefusalNote) *subscriptionLocalFallback {
	if s.subscriptionFallbackModel == "" || providers.IsLocalProvider(decision.Provider) ||
		isUserForcedReason(decision.Reason) || res.CallerModelPassthrough {
		return nil
	}
	exhaustedUnfunded := s.subscriptionExhaustedUnfunded(ctx, decision, headers)
	if !servedOnSubscription(ctx) && !exhaustedUnfunded && !managedSubscriptionCanServe(ctx, decision.Provider, decision.Model) {
		return nil
	}
	if !localModelServes(s.subscriptionFallbackProvider, s.subscriptionFallbackModel, req) {
		return nil
	}
	// Retarget a copy so routing metadata survives; the plan resolver authorizes
	// the local target under the deployment override source.
	target := decision
	target.Provider, target.Model = s.subscriptionFallbackProvider, s.subscriptionFallbackModel
	target.Effort = ""
	target.Reason = reasonSubscriptionLocalFallback
	return &subscriptionLocalFallback{
		target: target, original: decision, note: note, exhaustedUnfunded: exhaustedUnfunded,
		localFirst: exhaustedUnfunded && !managedSubscriptionCanServe(ctx, decision.Provider, decision.Model),
	}
}

// subscriptionExhaustedUnfunded reports a subscription the usage observer read
// spent whose provider has no paid key: pre-dispatch suppression leaves that
// turn without a credential, so the subscription has already refused it.
func (s *Service) subscriptionExhaustedUnfunded(ctx context.Context, decision router.Decision, headers http.Header) bool {
	switch decision.Provider {
	case providers.ProviderAnthropic:
		return s.anthropicSubscriptionObservedExhausted(ctx, headers) && !s.anthropicFallbackKeyAvailable(ctx)
	case providers.ProviderOpenAI:
		return codexSubscriptionCanAttemptModel(decision.Model) &&
			s.codexSubscriptionExhausted(ctx, headers) && !s.openaiFallbackKeyAvailable(ctx)
	}
	return false
}

// refusal returns the subscription refusal the fallback answers, or nil when
// err is not one: a noted limit refusal whose final error is still a capacity
// failure, an exhausted subscription pool, or a subscription already read spent.
// A request or credential rejection from a later paid retry surfaces as is.
func (fb *subscriptionLocalFallback) refusal(err error) error {
	if fb == nil || fb.done || err == nil {
		return nil
	}
	if fb.note != nil && fb.note.err != nil {
		if providers.IsRetryable(err) || isSubscriptionPoolError(err) || subscriptionLimitRefusal(err) {
			return fb.note.err
		}
		return nil
	}
	if errors.Is(err, ErrSubscriptionPoolExhausted) || fb.exhaustedUnfunded {
		return err
	}
	return nil
}

// servesFirst reports a turn the local model takes before any vendor dispatch.
func (fb *subscriptionLocalFallback) servesFirst() bool {
	return fb != nil && fb.localFirst
}

// unfundedRefusal is the limit refusal a servesFirst turn stands in for; the
// client sees it if the local model fails before output.
func (fb *subscriptionLocalFallback) unfundedRefusal() error {
	errType := "rate_limit_error"
	if fb.original.Provider == providers.ProviderOpenAI {
		errType = "usage_limit_reached"
	}
	return &providers.UpstreamErrorResponse{
		Status:  http.StatusTooManyRequests,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"error":{"type":"` + errType + `","message":"The ` + fb.original.Model + ` subscription usage limit is reached and no API key is configured to continue."}}`),
	}
}

// holds reports whether the deferred error must stay off the wire because the
// fallback may still serve the turn.
func (fb *subscriptionLocalFallback) holds(err error, buf *preludeBuffer) bool {
	return fb.refusal(err) != nil && !committed(buf)
}

// marker renders the routing badge naming the local model and the selection
// whose subscription refused the turn.
func (fb *subscriptionLocalFallback) marker(res turnLoopResult) string {
	if res.SuggestionMode {
		return ""
	}
	return routingMarkerPrefix + markerModelLabel(fb.target) + " · " +
		markerReasonSubscriptionLocalFallback + " " + fb.original.Model + " subscription limit\n\n"
}

// logServing records the fallback taking over a refused turn.
func (fb *subscriptionLocalFallback) logServing(ctx context.Context, res turnLoopResult, refusal error) {
	observability.FromContext(ctx).Warn("Subscription local fallback serving turn",
		"turn_type", string(res.TurnType),
		"original_model", fb.original.Model,
		"original_provider", fb.original.Provider,
		"original_reason", fb.original.Reason,
		"fallback_model", fb.target.Model,
		"fallback_provider", fb.target.Provider,
		"upstream_status", upstreamStatus(refusal),
		"err", refusal,
	)
}

// served records on res that the fallback replaced the original selection, so
// session-pin and HMM usage, and the policy outcome, keep the original pick
// and the next turn tries the subscription again.
func (fb *subscriptionLocalFallback) served(res *turnLoopResult) {
	res.SubstitutedFrom = fb.original
	res.SubstitutionReason = reasonSubscriptionLocalFallback
}
