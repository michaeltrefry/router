package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/cluster"
	"weave-os/router/internal/router/policy"
)

// ModelClassHeader restricts a request's automatic model selection to one
// catalog tier: high, mid or low.
const ModelClassHeader = "x-weave-model-class"

// ModelClassUnavailableCode is the error code a client receives when no model
// of the requested class can serve the call.
const ModelClassUnavailableCode = "model_class_unavailable"

// ModelClassContextKey carries the catalog.Tier parsed from ModelClassHeader.
type ModelClassContextKey struct{}

// ErrModelClassInvalid rejects a ModelClassHeader value that names no class.
var ErrModelClassInvalid = errors.New("invalid " + ModelClassHeader + " header: want high, mid or low")

// ParseModelClass resolves a ModelClassHeader value, case-insensitively.
func ParseModelClass(raw string) (catalog.Tier, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "high":
		return catalog.TierHigh, nil
	case "mid":
		return catalog.TierMid, nil
	case "low":
		return catalog.TierLow, nil
	default:
		return catalog.TierUnknown, ErrModelClassInvalid
	}
}

// WithModelClass restricts ctx's automatic model selection to class, and to
// members when the class has a strict order.
func WithModelClass(ctx context.Context, class catalog.Tier, members []string) context.Context {
	ctx = context.WithValue(ctx, ModelClassContextKey{}, class)
	if len(members) == 0 {
		return ctx
	}
	set := make(map[string]struct{}, len(members))
	for _, m := range members {
		set[m] = struct{}{}
	}
	return context.WithValue(ctx, ModelClassMembersContextKey{}, set)
}

type modelClassImpliedContextKey struct{}

// WithRequestedModelClass names the tiers whose requested models are served
// in their own class, as if the request sent ModelClassHeader.
func (s *Service) WithRequestedModelClass(classes ...catalog.Tier) *Service {
	s.requestedModelClass = map[catalog.Tier]bool{}
	for _, c := range classes {
		s.requestedModelClass[c] = true
	}
	return s
}

// impliedModelClass holds a request whose requested model is of a
// WithRequestedModelClass tier to that class, so the router never serves it
// on a model of another tier. A request that names a class, forces a model or
// passes the caller's model through is left alone; a saved session force
// still outranks the implied class (modelClassImplied).
func (s *Service) impliedModelClass(ctx context.Context, requestedModel, forceModel string) context.Context {
	if _, classed := requestModelClass(ctx); classed || forceModel != "" || callerModelPassthroughActive(ctx) {
		return ctx
	}
	class := catalog.TierFor(requestedModel)
	if !s.requestedModelClass[class] {
		return ctx
	}
	ctx = WithModelClass(ctx, class, s.ModelClassMembers(class))
	return context.WithValue(ctx, modelClassImpliedContextKey{}, true)
}

// modelClassImplied reports whether the request's class came from its
// requested model rather than ModelClassHeader.
func modelClassImplied(ctx context.Context) bool {
	implied, _ := ctx.Value(modelClassImpliedContextKey{}).(bool)
	return implied
}

// requestModelClass returns the class the request asked for, if any.
func requestModelClass(ctx context.Context) (catalog.Tier, bool) {
	class, ok := ctx.Value(ModelClassContextKey{}).(catalog.Tier)
	return class, ok && class != catalog.TierUnknown
}

// modelInRequestedClass reports whether model belongs to the request's class;
// true when the request named none.
func modelInRequestedClass(ctx context.Context, model string) bool {
	class, ok := requestModelClass(ctx)
	return !ok || catalog.TierFor(model) == class && classMembersAllow(ctx, model)
}

// forcedModelClassConflict names why a forced model cannot serve a request
// that asked for another class, or returns "" when it can. A force and a
// class are both explicit caller choices, so a conflict fails the request.
func forcedModelClassConflict(ctx context.Context, model string) string {
	class, ok := requestModelClass(ctx)
	if !ok || modelClassImplied(ctx) || catalog.TierFor(model) == class {
		return ""
	}
	return fmt.Sprintf("%s is a %s-class model; this request asked for %s: %s", model, catalog.TierFor(model), ModelClassHeader, class)
}

// ModelClassUnavailableError reports that no model of Class can serve the
// request; the router never serves it on another class instead.
type ModelClassUnavailableError struct {
	Class catalog.Tier
	Err   error
}

// Error implements error.
func (e *ModelClassUnavailableError) Error() string {
	return fmt.Sprintf("%s: no %s-class model can serve this request: %v", ModelClassUnavailableCode, e.Class, e.Err)
}

// Unwrap returns the routing error that emptied the class.
func (e *ModelClassUnavailableError) Unwrap() error { return e.Err }

// modelClassUnavailable wraps a routing error that left no eligible candidate
// when the request named a class, so the client learns the class is the cause.
func modelClassUnavailable(ctx context.Context, err error) error {
	class, ok := requestModelClass(ctx)
	if !ok || err == nil || !errors.Is(err, cluster.ErrNoEligibleProvider) && !errors.Is(err, policy.ErrNoRoutableModels) {
		return err
	}
	// The org allowlist names its own fix; the class is not the cause.
	if errors.Is(err, cluster.ErrAllowlistEmptiesPool) {
		return err
	}
	var already *ModelClassUnavailableError
	if errors.As(err, &already) {
		return err
	}
	return &ModelClassUnavailableError{Class: class, Err: err}
}

// modelClassExclusions returns every routable model outside the requested
// class, or nil when the request named none.
func (s *Service) modelClassExclusions(ctx context.Context) map[string]struct{} {
	class, ok := requestModelClass(ctx)
	if !ok {
		return nil
	}
	// The routable universe omits untiered models that mapping targets and
	// operator hard pins can still name, so every catalog model is judged.
	out := map[string]struct{}{}
	for model := range s.routableUniverse() {
		if catalog.TierFor(model) != class || !classMembersAllow(ctx, model) {
			out[model] = struct{}{}
		}
	}
	for _, m := range catalog.Models {
		if m.Tier != class || !classMembersAllow(ctx, m.ID) {
			out[m.ID] = struct{}{}
		}
	}
	return out
}

// setModelClassHeader names the served model's class on the response; a
// model without a tier sets nothing.
func setModelClassHeader(h http.Header, model string) {
	if tier := catalog.TierFor(model); tier != catalog.TierUnknown {
		h.Set(HeaderRouterModelClass, tier.String())
		return
	}
	h.Del(HeaderRouterModelClass)
}
