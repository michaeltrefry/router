package proxy

import (
	"context"
	"net/http"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/translate"
)

// turnRescues is the paid and peer rescue eligibility of one routed target,
// judged under the credential context that target resolved. A turn re-served
// on another target recomputes it, since the original target's credentials
// say nothing about the new one's.
type turnRescues struct {
	// claudeRetry: a Claude subscription turn may retry on a paid Anthropic key.
	claudeRetry bool
	// codexRetry: a ChatGPT plan turn may retry on a paid OpenAI key.
	codexRetry bool
	// siblings lists the same-cluster stand-ins; siblingViable licenses them.
	siblings      []router.Decision
	siblingViable bool
}

// turnRescuesFor computes decision's rescues under ctx, the context its
// credentials resolved into. Callers add their surface's own gates (shadow
// evaluation, blind experiments, force-model reasons).
func (s *Service) turnRescuesFor(ctx context.Context, decision router.Decision, res turnLoopResult, est, sigSavings, outputReserve int) turnRescues {
	paidAllowed := !paidFallbackForbidden(ctx)
	siblings := s.siblingFailoverDecisions(ctx, rescueBasisForTurn(decision, res), est, sigSavings, outputReserve)
	return turnRescues{
		claudeRetry: decision.Provider == providers.ProviderAnthropic &&
			servedOnSubscription(ctx) && paidAllowed && s.anthropicFallbackKeyAvailable(ctx),
		codexRetry: decision.Provider == providers.ProviderOpenAI &&
			servedOnCodexSubscription(ctx) && paidAllowed && s.openaiFallbackKeyAvailable(ctx),
		siblings: siblings,
		siblingViable: s.ResolveSiblingFailover(ctx) &&
			len(siblings) > 0 &&
			!res.CallerModelPassthrough &&
			(s.shouldFailover(ctx) || s.gatewaySiblingAllowed(ctx, siblings[0])) &&
			paidAllowed,
	}
}

// openAITurnSurface is an OpenAI-ingress target's resolved credentials and
// upstream surface.
type openAITurnSurface struct {
	ctx context.Context
	// responses routes the target to the OpenAI Responses API.
	responses bool
	// endpointKey names the gateway whose Responses support is memoized.
	endpointKey string
}

// resolveOpenAITurnSurface resolves decision's credentials and decides whether
// it is served on the Responses API: a reasoning model with tools, or a Codex
// subscription transport, unless the gateway is known to lack the surface.
// passthrough means the caller's own Responses bytes already go there.
func (s *Service) resolveOpenAITurnSurface(ctx context.Context, env *translate.RequestEnvelope, decision router.Decision, caps router.ModelSpec, hasTools, passthrough bool, headers http.Header) (openAITurnSurface, error) {
	resolvedCtx := s.resolveCredentials(ctx, decision.Provider, decision.Model, headers)
	surface := openAITurnSurface{endpointKey: EffectiveBaseURL(resolvedCtx, decision.Provider), responses: passthrough}
	if !surface.responses && decision.Provider == providers.ProviderOpenAI {
		chatOnly := env.RequiresChatCompletionsParams(caps)
		gatewayLacks := s.gatewayLacksResponses(surface.endpointKey)
		surface.responses = translate.UseOpenAIResponsesAPI(translate.ResponsesRoute{
			Provider:       decision.Provider,
			Capabilities:   caps,
			HasTools:       hasTools,
			ChatOnlyParams: chatOnly,
			Broad:          s.ResolveOpenAIResponsesBroad(ctx),
		}) && !gatewayLacks
		if !chatOnly && !gatewayLacks && s.includedOnlySubscriptionTransport(decision.Provider) &&
			(servedOnCodexSubscription(resolvedCtx) || managedSubscriptionCanServe(ctx, decision.Provider, decision.Model)) {
			surface.responses = true
		}
	}
	endpointCtx, err := s.avoidCodexOnChatEndpoint(ctx, decision.Provider, decision.Model, surface.responses, headers)
	if err != nil {
		return openAITurnSurface{}, err
	}
	if codexChatEndpoint(endpointCtx) {
		surface.ctx = s.resolveCredentials(endpointCtx, decision.Provider, decision.Model, headers)
	} else {
		surface.ctx = resolvedCtx
	}
	return surface, nil
}
