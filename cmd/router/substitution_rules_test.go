package main

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/router/sessionpin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestParseLocalModels_SubstitutionRules(t *testing.T) {
	env := envFrom(map[string]string{"KEY_A": "a"})
	entries := "models:\n" + localEntryYAML("m1", "http://localhost:1/v1", "KEY_A")
	parse := func(t *testing.T, block string) (localModelsConfig, error) {
		t.Helper()
		return parseLocalModels(strings.NewReader(entries+block), env)
	}

	t.Run("omitted block substitutes nothing", func(t *testing.T) {
		cfg, err := parse(t, "")
		require.NoError(t, err)
		assert.Empty(t, cfg.substitutionRules)
	})
	t.Run("rules load in order", func(t *testing.T) {
		cfg, err := parse(t, "substitution_rules:\n  - match: gpt-*-terra\n    model: m1\n  - match: gpt-6-luna\n    model: m1\n")
		require.NoError(t, err)
		provider := providers.LocalProviderName("m1")
		assert.Equal(t, []proxy.SubstitutionRule{
			{Match: "gpt-*-terra", Provider: provider, Model: "m1"},
			{Match: "gpt-6-luna", Provider: provider, Model: "m1"},
		}, cfg.substitutionRules)
	})
	rejected := []struct {
		name  string
		block string
		want  error
	}{
		{"unconfigured local model", "substitution_rules:\n  - match: gpt-*-luna\n    model: qwen-x\n", errSubstitutionRuleModel},
		{"catalog model as target", "substitution_rules:\n  - match: gpt-*-luna\n    model: gpt-6-luna\n", errSubstitutionRuleModel},
		{"invalid glob", "substitution_rules:\n  - match: gpt-[\n    model: m1\n", errSubstitutionRuleGlob},
		{"glob naming no catalog model", "substitution_rules:\n  - match: gpt-*-lunaa\n    model: m1\n", errSubstitutionRuleNoMatch},
		{"empty match", "substitution_rules:\n  - model: m1\n", errSubstitutionRuleNoMatch},
	}
	for _, tc := range rejected {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			_, err := parse(t, tc.block)
			require.ErrorIs(t, err, tc.want)
		})
	}
	t.Run("rejects an unknown rule field", func(t *testing.T) {
		_, err := parse(t, "substitution_rules:\n  - match_tier: mid\n    model: m1\n")
		require.Error(t, err)
	})
}

// lunaTerraYAML is the shipped Luna/Terra rule set plus the shipped Codex
// scorer mapping, pointed at local model id.
func lunaTerraYAML(id string) string {
	return "model_mapping:\n  gpt-5.4-mini: gpt-6-luna\n" +
		"substitution_rules:\n  - match: gpt-*-luna\n    model: " + id + "\n  - match: gpt-*-terra\n    model: " + id + "\n"
}

// lunaTerraService boots a local-models file through the composition root next
// to an OpenAI client that serves Codex subscription turns natively.
func lunaTerraService(t *testing.T, id, entryYAML, extraYAML string, scorer *countingRouter, pins sessionpin.Store) (*proxy.Service, *streamingOpenAI) {
	t.Helper()
	path := writeLocalModelsFile(t, entryYAML+extraYAML)
	openAIClient := &streamingOpenAI{}
	providerMap := map[string]providers.Client{providers.ProviderOpenAI: openAIClient}
	keyed := map[string]struct{}{providers.ProviderOpenAI: {}}
	cfg, err := loadLocalModels(
		envFrom(map[string]string{localModelsFileEnv: path, "LOCAL_TEST_KEY": "local-secret"}),
		providerMap, keyed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		catalog.UnregisterLocalModels(id)
		provider := providers.LocalProviderName(id)
		delete(providers.ProviderFamilies, provider)
		delete(providers.APIKeyEnvVars, provider)
	})
	svc := proxy.NewService(scorer, providerMap, nil, false, nil, pins, false, providers.ProviderOpenAI, "gpt-5.6-luna", nil).
		WithDeploymentKeyedProviders(keyed).
		WithMidTierSubstitute(cfg.midTier).
		WithSubstitutionRules(cfg.substitutionRules).
		WithModelMapping(cfg.modelMapping)
	return svc, openAIClient
}

var gpt54MiniPick = router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.4-mini", Reason: "scored"}

// A Codex Responses turn the scorer gave to gpt-5.4-mini is mapped to
// gpt-6-luna and served by the local model, with the local model's reasoning
// and tool call surfacing as Responses items. The session keeps the scorer's
// pick, so the next turn is substituted again without a new badge.
func TestSubstitutionRule_CodexMiniPickServedLocally(t *testing.T) {
	const id = "test-rule-codex-mini"
	toolStream := chunk(`{"role":"assistant","content":null,"reasoning_content":"Need to read the file."}`, "") +
		chunk(`{"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"shell","arguments":"{\"command\":[\"cat\",\"go.mod\"]}"}}]}`, "") +
		`data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}` + "\n\n" +
		"data: [DONE]\n\n"
	upstream := newScriptedLocalUpstream(t, toolStream)
	pins := &memoryPins{pins: map[string]sessionpin.Pin{}}
	svc, openAIClient := lunaTerraService(t, id, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY"), lunaTerraYAML(id),
		&countingRouter{decision: gpt54MiniPick}, pins)

	var outs []string
	for range 2 {
		ctx, r := codexRequest(t, nil)
		rec := httptest.NewRecorder()
		require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexToolTurn), rec, r))
		require.Equal(t, http.StatusOK, rec.Code)
		outs = append(outs, rec.Body.String())
	}

	assert.Empty(t, openAIClient.served(), "neither turn reaches the Codex subscription")
	upstream.mu.Lock()
	require.Len(t, upstream.bodies, 2)
	assert.Equal(t, "upstream-"+id, gjson.GetBytes(upstream.bodies[0], "model").String())
	assert.Equal(t, "Bearer local-secret", upstream.authz[0], "the configured key authenticates, never the caller's ChatGPT token")
	upstream.mu.Unlock()

	events := parseClientSSE(t, outs[0])
	assert.Equal(t, "response.completed", events[len(events)-1].name)
	var reasoning, call gjson.Result
	for _, item := range responsesItems(t, events) {
		switch item.Get("type").String() {
		case "reasoning":
			reasoning = item
		case "function_call":
			call = item
		}
	}
	require.True(t, reasoning.Exists(), "the local reasoning surfaces as a reasoning item: %s", outs[0])
	assert.Contains(t, reasoning.Get("summary.#.text").String()+reasoning.Get("content.#.text").String(), "Need to read the file.")
	require.True(t, call.Exists(), "the tool call surfaces as a function_call item: %s", outs[0])
	assert.Equal(t, "shell", call.Get("name").String())
	assert.Contains(t, outs[0], "→ "+id+" (local) · substitute for gpt-6-luna (mapped from gpt-5.4-mini)")
	assert.Zero(t, badgeCount(outs[1]), "the session's pick is unchanged: no badge on the next turn")

	pins.mu.Lock()
	defer pins.mu.Unlock()
	require.NotEmpty(t, pins.pins)
	for _, pin := range pins.pins {
		assert.Equal(t, "gpt-5.4-mini", pin.Model, "the session pin names the scorer's pick")
		assert.Equal(t, "gpt-5.4-mini", pin.LastServedModel)
	}
}

// When the local model cannot take the turn, or fails before output, a Luna or
// Terra turn is served by the matched OpenAI model on the caller's Codex
// subscription.
func TestSubstitutionRule_CodexLunaTerraFallBackToMatchedModel(t *testing.T) {
	cases := []struct {
		name   string
		pick   router.Decision
		served string
		fail   bool
	}{
		{name: "mapped luna, local fails", pick: gpt54MiniPick, served: "gpt-6-luna", fail: true},
		{name: "terra, local fails", pick: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-terra", Reason: "scored"}, served: "gpt-5.6-terra", fail: true},
		{name: "mapped luna, local ineligible", pick: gpt54MiniPick, served: "gpt-6-luna"},
		{name: "terra, local ineligible", pick: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-terra", Reason: "scored"}, served: "gpt-5.6-terra"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const id = "test-rule-codex-fb"
			local := newFailingLocal(t, "500")
			entry := localEntryYAML(id, local.baseURL, "LOCAL_TEST_KEY")
			if !tc.fail {
				// A low tool-use model never takes a tool-bearing turn.
				entry += "    tool_use: low\n"
			}
			svc, openAIClient := lunaTerraService(t, id, entry, lunaTerraYAML(id), &countingRouter{decision: tc.pick}, nil)
			ctx, r := codexRequest(t, nil)
			var logs bytes.Buffer
			ctx = observability.WithLogger(ctx, slog.New(slog.NewJSONHandler(&logs, nil)))
			rec := httptest.NewRecorder()

			require.NoError(t, svc.ProxyOpenAIResponses(ctx, []byte(codexMainTurn), rec, r))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.Equal(t, []string{tc.served}, openAIClient.served())
			oauth, _ := openAIClient.calls()
			assert.Equal(t, []bool{true}, oauth, "the matched model runs on the caller's Codex subscription")
			assert.Contains(t, rec.Body.String(), "normal route answer")
			assert.Equal(t, 1, badgeCount(rec.Body.String()))
			if tc.fail {
				assert.Positive(t, local.count(), "the local model is tried first")
				fallback := logLine(t, &logs, "Local model failed before output; serving the turn on its normal route")
				assert.Equal(t, tc.served, fallback["fallback_model"])
				assert.Equal(t, "substitution_rule", fallback["local_source"])
				return
			}
			assert.Zero(t, local.count(), "an ineligible local model is never dispatched")
			logLine(t, &logs, "Local substitute skipped; serving the matched model")
		})
	}
}

// An eligible Terra pick is served locally on Chat Completions too.
func TestSubstitutionRule_TerraPickServedLocallyOnChatCompletions(t *testing.T) {
	const id = "test-rule-chat-terra"
	upstream := newLocalUpstream(t)
	svc, openAIClient := lunaTerraService(t, id, localEntryYAML(id, upstream.baseURL, "LOCAL_TEST_KEY"), lunaTerraYAML(id),
		&countingRouter{decision: router.Decision{Provider: providers.ProviderOpenAI, Model: "gpt-5.6-terra", Reason: "scored"}}, nil)
	rec := httptest.NewRecorder()

	require.NoError(t, svc.ProxyOpenAIChatCompletion(routerKeyedCtx(), []byte(chatCompletionsTurn), rec, chatCompletionsRequest()))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, openAIClient.served())
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.bodies, 1)
	assert.Contains(t, rec.Body.String(), "substitute for gpt-5.6-terra")
}
