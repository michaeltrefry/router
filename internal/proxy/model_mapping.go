package proxy

import (
	"context"
	"maps"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/policy"
)

// reasonModelMapping names the model mapping as what replaced a turn's
// automatic selection.
const reasonModelMapping = "model_mapping"

// ModelMapping maps an automatically selected catalog model to the catalog
// model that serves it, e.g. a scorer roster model to its family's current
// release.
type ModelMapping map[string]string

// WithModelMapping installs the deployment's model mapping; an empty mapping
// serves every selection as chosen.
func (s *Service) WithModelMapping(mapping ModelMapping) *Service {
	s.modelMapping = maps.Clone(mapping)
	return s
}

// mapModel retargets the router's automatic selection onto its mapped model.
// It runs before the substitution rules and shares their eligibility: forced,
// hard-pinned, utility, bypassed, classifier, compaction and policy-pinned
// turns are dispatched unmapped, as is a turn whose target cannot serve req.
// Session pins, HMM history and the policy outcome keep the router's own pick
// through SubstitutedFrom.
func (s *Service) mapModel(ctx context.Context, res *turnLoopResult, req router.Request) {
	if len(s.modelMapping) == 0 || !midTierSubstitutable(*res) {
		return
	}
	original := res.Decision
	target, mapped := s.modelMapping[original.Model]
	if !mapped {
		return
	}
	if _, pinned := router.HonouredPolicyPin(ctx); pinned {
		return
	}
	// Retarget a copy of the routed decision, keeping its effort; the arm
	// selection names an upstream of the original model, so it is dropped.
	// The plan resolver authorizes the new target under the deployment
	// override source.
	mappedDecision := original
	mappedDecision.Model = target
	mappedDecision.Provider = mappedProvider(target, original.Provider)
	mappedDecision.Metadata = withoutArmSelection(original.Metadata)
	if reason := s.mappingTargetIneligibility(ctx, mappedDecision, req); reason != "" {
		observability.FromContext(ctx).Info("Model mapping skipped; serving the trained model",
			"reason", reason,
			"turn_type", string(res.TurnType),
			"original_model", original.Model,
			"mapped_model", target,
			"mapped_provider", mappedDecision.Provider,
		)
		return
	}
	res.SubstitutedFrom = original
	res.SubstitutionReason = reasonModelMapping
	res.MappedDecision = mappedDecision
	res.Decision = mappedDecision
	res.Origin = policy.OverrideSourceDeployment
	observability.FromContext(ctx).Debug("Model mapping retargeted turn",
		"turn_type", string(res.TurnType),
		"original_model", original.Model,
		"original_provider", original.Provider,
		"mapped_model", target,
		"mapped_provider", mappedDecision.Provider,
	)
}

// mappingTargetIneligibility names why req may not be served on the mapped
// target, or returns "" when it may: neither the model nor its provider is
// excluded, the model clears every positive allowlist, the provider has a
// registered dispatch client, and the target passes the request checks every
// model the router serves on its own behalf passes (provider enrolled for this
// request, images, context window, tool ratings, automatic disable). An
// ineligible target serves the router's own pick instead.
func (s *Service) mappingTargetIneligibility(ctx context.Context, target router.Decision, req router.Request) string {
	if _, excluded := s.excludedModelsForRequest(ctx)[target.Model]; excluded {
		return "excluded"
	}
	// The desugared exclusion set covers only the routable universe, which
	// need not contain a mapping target.
	if allowed := allowedModelsForRequest(ctx); allowed != nil {
		if _, ok := allowed[target.Model]; !ok {
			return "not_allowed"
		}
	}
	if _, excluded := s.excludedProvidersForRequest(ctx)[target.Provider]; excluded {
		return "provider_excluded"
	}
	if !s.clients.Has(target.Provider) {
		return "no_dispatch_client"
	}
	return automaticServingIneligibility(target.Provider, target.Model, req)
}

// mappedProvider keeps the selection's provider when the target is bound to
// it, else uses the target's primary binding.
func mappedProvider(target, provider string) string {
	model, _ := catalog.ByID(target)
	for _, binding := range model.Providers {
		if binding.Provider == provider {
			return provider
		}
	}
	return model.PrimaryProvider()
}

// applyServingRules maps then substitutes the turn's automatic selection.
func (s *Service) applyServingRules(ctx context.Context, res *turnLoopResult, req router.Request) {
	s.mapModel(ctx, res, req)
	s.substituteLocal(ctx, res, req)
}

type mappingAdmittedContextKey struct{}

// withMappingAdmitted records the models excludeCodexOAuthOnlyModels kept
// selectable only because their mapped target is subscription-servable.
func withMappingAdmitted(ctx context.Context, admitted map[string]struct{}) context.Context {
	if len(admitted) == 0 && ctx.Value(mappingAdmittedContextKey{}) == nil {
		return ctx
	}
	return context.WithValue(ctx, mappingAdmittedContextKey{}, admitted)
}

// unservedMappingAdmission returns the request's mapping-admitted models when
// res would dispatch one of them unmapped, directly or as a local
// substitute's normal route, else nil. Such a model has no credential that
// may serve it on this request.
func unservedMappingAdmission(ctx context.Context, res turnLoopResult) map[string]struct{} {
	admitted, _ := ctx.Value(mappingAdmittedContextKey{}).(map[string]struct{})
	if len(admitted) == 0 {
		return nil
	}
	dispatched := res.Decision.Model
	if localSubstitutionReason(res.SubstitutionReason) && res.MappedDecision.Model == "" {
		dispatched = res.SubstitutedFrom.Model
	}
	if _, unserved := admitted[dispatched]; !unserved {
		return nil
	}
	return admitted
}
