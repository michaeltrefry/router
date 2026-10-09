package proxy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/turntype"
)

// The subscription fallback never re-serves a local model that already failed
// earlier in the same turn's chain, not only the one that failed last.
func TestSubscriptionFallbackAfterLocalFailure_SkipsEveryFailedLocal(t *testing.T) {
	mimo := router.Decision{Provider: providers.LocalProviderName("mimo"), Model: "mimo"}
	qwen := router.Decision{Provider: providers.LocalProviderName("qwen"), Model: "qwen"}
	svc := (&Service{}).WithSubscriptionLocalFallback(SubscriptionLocalFallback{Provider: mimo.Provider, Model: mimo.Model})
	ctx, _ := withSubscriptionRefusalNote(context.WithValue(context.Background(), CredentialsContextKey{}, &Credentials{OAuth: true}))
	normal := turnLoopResult{TurnType: turntype.MainLoop, Decision: router.Decision{Provider: providers.ProviderAnthropic, Model: "claude-haiku-4-5"}}

	onlyQwen := &localFailureFallback{local: qwen}
	require.NotNil(t, svc.planSubscriptionLocalFallbackAfterLocalFailure(ctx, onlyQwen, normal, router.Request{}, nil),
		"precondition: with only qwen failed, the fallback plans mimo")
	chain := (&localFailureFallback{local: qwen}).after(&localFailureFallback{local: mimo})
	assert.Nil(t, svc.planSubscriptionLocalFallbackAfterLocalFailure(ctx, chain, normal, router.Request{}, nil))
}
