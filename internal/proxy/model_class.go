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

// requestModelClass returns the class the request asked for, if any.
func requestModelClass(ctx context.Context) (catalog.Tier, bool) {
	class, ok := ctx.Value(ModelClassContextKey{}).(catalog.Tier)
	return class, ok && class != catalog.TierUnknown
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
	out := map[string]struct{}{}
	for model := range s.routableUniverse() {
		if catalog.TierFor(model) != class {
			out[model] = struct{}{}
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
