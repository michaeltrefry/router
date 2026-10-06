package translate_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"weave-os/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const toolChangesBody = `{
	"model": "claude-opus-4-8",
	"max_tokens": 1024,
	"tools": [
		{"name": "Read", "description": "Read a file", "input_schema": {"type": "object"}},
		{"name": "Edit", "description": "Edit a file", "input_schema": {"type": "object"}},
		{"name": "Grep", "description": "Search", "input_schema": {"type": "object"}},
		{"name": "mcp__db__query", "description": "Run SQL", "defer_loading": true, "input_schema": {"type": "object"}}
	],
	"messages": [
		{"role": "user", "content": [
			{"type": "tool_addition", "tool": {"type": "tool_reference", "name": "mcp__db__query"}},
			{"type": "text", "text": "start"}
		]},
		{"role": "system", "content": [
			{"type": "tool_removal", "tool": {"type": "tool_reference", "name": "Edit"}},
			{"type": "tool_removal", "tool": {"type": "tool_reference", "name": "Grep"}},
			{"type": "tool_addition", "tool": {"name": "mcp__metrics__get", "description": "Fetch a metric", "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}}}}
		]},
		{"role": "assistant", "content": [
			{"type": "tool_addition", "tool": {"type": "tool_reference", "name": "Grep"}},
			{"type": "text", "text": "ok"}
		]},
		{"role": "user", "content": "go"}
	]
}`

var toolChangesWant = []string{"Read", "Grep", "mcp__db__query", "mcp__metrics__get"}

func TestPrepareOpenAI_AppliesToolChangeBlocksToTools(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(toolChangesBody))
	require.NoError(t, err)
	out, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{TargetModel: "local-model"})
	require.NoError(t, err)

	assert.Equal(t, toolChangesWant, emittedToolNames(t, out.Body),
		"removed tools leave, a re-added tool returns, and an inline-defined addition is appended in order")
	metrics := gjson.GetBytes(out.Body, `tools.#(function.name=="mcp__metrics__get").function`)
	assert.Equal(t, "Fetch a metric", metrics.Get("description").String())
	assert.Equal(t, "string", metrics.Get("parameters.properties.name.type").String())

	for _, msg := range gjson.GetBytes(out.Body, "messages").Array() {
		assert.NotEqual(t, "system", msg.Get("role").String(), "no system entry without a top-level system prompt: %s", msg.Raw)
	}
	for _, leaked := range []string{"tool_addition", "tool_removal", "tool_reference", "defer_loading"} {
		assert.NotContains(t, string(out.Body), leaked)
	}
}

func TestPrepareOpenAIResponses_AppliesToolChangeBlocksToTools(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(toolChangesBody))
	require.NoError(t, err)
	out, err := env.PrepareOpenAIResponses(http.Header{}, translate.EmitOptions{TargetModel: "gpt-5.6-luna"})
	require.NoError(t, err)
	assert.Equal(t, toolChangesWant, emittedResponsesToolNames(t, out.Body))
	assert.NotContains(t, string(out.Body), "tool_addition")
}

func TestPrepareGemini_AppliesToolChangeBlocksToTools(t *testing.T) {
	env, err := translate.ParseAnthropic([]byte(toolChangesBody))
	require.NoError(t, err)
	out, err := env.PrepareGemini(http.Header{}, translate.EmitOptions{TargetModel: "gemini-3.1-pro-preview"})
	require.NoError(t, err)
	assert.Equal(t, toolChangesWant, emittedGeminiToolNames(t, out.Body))
	assert.NotContains(t, string(out.Body), "tool_addition")
}

func TestPrepareOpenAI_ToolsUnchangedWithoutToolChangeBlocks(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"tools":[{"name":"Read","input_schema":{"type":"object"}},{"name":"Edit","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	out, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{TargetModel: "local-model"})
	require.NoError(t, err)
	assert.Equal(t, []string{"Read", "Edit"}, emittedToolNames(t, out.Body))
}

// OpenAI caps tools at 128; a deferred tool listed past the cap must survive
// truncation once the conversation adds it.
func TestPrepareOpenAI_AddedToolSurvivesToolCap(t *testing.T) {
	tools := make([]string, 0, 131)
	for i := range 130 {
		tools = append(tools, fmt.Sprintf(`{"name":"tool_%03d","input_schema":{"type":"object"}}`, i))
	}
	tools = append(tools, `{"name":"mcp__late","defer_loading":true,"input_schema":{"type":"object"}}`)
	body := `{"model":"m","max_tokens":10,"tools":[` + strings.Join(tools, ",") + `],"messages":[` +
		`{"role":"user","content":[{"type":"tool_addition","tool":{"type":"tool_reference","name":"mcp__late"}},{"type":"text","text":"go"}]}]}`
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	out, err := env.PrepareOpenAI(http.Header{}, translate.EmitOptions{TargetModel: "local-model"})
	require.NoError(t, err)

	names := emittedToolNames(t, out.Body)
	require.Len(t, names, 128)
	assert.Equal(t, "tool_000", names[0])
	assert.Equal(t, "tool_126", names[126])
	assert.Equal(t, "mcp__late", names[127])
}
