package proxy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
)

// markerReasonLocalFailure follows the failed local model's label in the
// routing marker of a turn its normal route served instead.
const markerReasonLocalFailure = "failed"

// localFailureSourceTurnRoute names the local turn route as the rule that put
// a turn on a local model; the mid-tier substitute uses reasonMidTierSubstitute.
const localFailureSourceTurnRoute = "local_turn_route"

// errNoNormalRoute reports a local turn whose routing without local rules
// lands on a local model again, so there is nothing else to serve it.
var errNoNormalRoute = errors.New("normal routing selects a local model")

type localRoutingDisabledKey struct{}

// withLocalRoutingDisabled marks ctx so the turn loop routes as if no local
// turn route or mid-tier substitute were configured.
func withLocalRoutingDisabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, localRoutingDisabledKey{}, true)
}

func localRoutingDisabled(ctx context.Context) bool {
	disabled, _ := ctx.Value(localRoutingDisabledKey{}).(bool)
	return disabled
}

// localFailureFallback is one turn's plan for a local model that fails before
// any output: serve the turn on the target its routing would have chosen
// without the local rule. The mid-tier substitute already holds that target;
// a local turn route recomputes it only when the local model has failed, so
// the common path never pays for a second routing pass.
type localFailureFallback struct {
	local   router.Decision
	source  string
	normal  *turnLoopResult
	reroute func() (turnLoopResult, error)
}

// planLocalFailureFallback returns the fallback plan for a turn the local turn
// route or the mid-tier substitute put on a local model. A user-forced local
// model, a caller-model passthrough and any other selection get nil: the
// error surfaces as it would without the plan. reroute re-runs the turn loop
// under withLocalRoutingDisabled; nil leaves a local turn route without one.
func planLocalFailureFallback(res turnLoopResult, reroute func() (turnLoopResult, error)) *localFailureFallback {
	if !providers.IsLocalProvider(res.Decision.Provider) || isUserForcedReason(res.Decision.Reason) || res.CallerModelPassthrough {
		return nil
	}
	switch {
	case res.SubstitutionReason == reasonMidTierSubstitute && res.SubstitutedFrom.Model != "":
		// Substitution only replaces router-selected, non-hard-pinned
		// decisions, which carry no turn-loop origin.
		normal := res
		normal.Decision = res.SubstitutedFrom
		normal.SubstitutedFrom = router.Decision{}
		normal.SubstitutionReason = ""
		normal.Origin = ""
		return &localFailureFallback{local: res.Decision, source: reasonMidTierSubstitute, normal: &normal}
	case res.LocalTurnRouted && reroute != nil:
		return &localFailureFallback{local: res.Decision, source: localFailureSourceTurnRoute, reroute: reroute}
	}
	return nil
}

// rescues reports whether err is a local failure the plan answers: any
// failure before the stream committed. A committed stream is never re-served.
func (fb *localFailureFallback) rescues(ctx context.Context, err error, buf *preludeBuffer) bool {
	return fb != nil && err != nil && !committed(buf) && ctx.Err() == nil
}

// normalRoute returns the turn as routing would have produced it without the
// local rule.
func (fb *localFailureFallback) normalRoute() (turnLoopResult, error) {
	if fb.normal != nil {
		return *fb.normal, nil
	}
	res, err := fb.reroute()
	if err != nil {
		return turnLoopResult{}, err
	}
	if res.Decision.Model == "" || providers.IsLocalProvider(res.Decision.Provider) {
		return turnLoopResult{}, fmt.Errorf("%w: %s", errNoNormalRoute, res.Decision.Model)
	}
	return res, nil
}

// marker renders the routing badge naming the model that served and the local
// model that failed. It follows the normal route's own badge rules, so a turn
// whose badge is hidden (hard pins, recap and classifier turns, whose output a
// harness parses) keeps none; the log line still records the fallback.
func (fb *localFailureFallback) marker(res turnLoopResult) string {
	normal := strings.TrimSuffix(routingMarkerFor(res), "\n\n")
	if normal == "" {
		return ""
	}
	return normal + " · " + markerModelLabel(fb.local) + " " + markerReasonLocalFailure + "\n\n"
}

// logServing records the normal route taking over a failed local turn.
func (fb *localFailureFallback) logServing(ctx context.Context, res turnLoopResult, localErr error) {
	observability.FromContext(ctx).Warn("Local model failed before output; serving the turn on its normal route",
		"turn_type", string(res.TurnType),
		"local_model", fb.local.Model,
		"local_provider", fb.local.Provider,
		"local_source", fb.source,
		"fallback_model", res.Decision.Model,
		"fallback_provider", res.Decision.Provider,
		"fallback_reason", res.Decision.Reason,
		"upstream_status", upstreamStatus(localErr),
		"err", localErr,
	)
}

// logUnavailable records a failed local turn the normal route cannot take.
func (fb *localFailureFallback) logUnavailable(ctx context.Context, localErr, why error) {
	observability.FromContext(ctx).Warn("Local model failed before output and its normal route is unavailable; surfacing the local error",
		"local_model", fb.local.Model,
		"local_provider", fb.local.Provider,
		"local_source", fb.source,
		"upstream_status", upstreamStatus(localErr),
		"err", localErr,
		"normal_route_err", why,
	)
}
