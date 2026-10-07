package main

import (
	"bufio"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type clientSSEEvent struct {
	name string
	data gjson.Result
}

func parseClientSSE(t *testing.T, body string) []clientSSEEvent {
	t.Helper()
	var events []clientSSEEvent
	var name string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			require.True(t, gjson.Valid(data), "client frame must be JSON: %s", data)
			events = append(events, clientSSEEvent{name: name, data: gjson.Parse(data)})
			name = ""
		}
	}
	require.NoError(t, scanner.Err())
	return events
}

// contentBlocks reassembles the Anthropic content blocks a client would see.
func contentBlocks(t *testing.T, events []clientSSEEvent) []map[string]string {
	t.Helper()
	var blocks []map[string]string
	for _, ev := range events {
		switch ev.name {
		case "error":
			t.Fatalf("stream carried an error event: %s", ev.data.Raw)
		case "content_block_start":
			cb := ev.data.Get("content_block")
			blocks = append(blocks, map[string]string{
				"type": cb.Get("type").String(),
				"id":   cb.Get("id").String(),
				"name": cb.Get("name").String(),
				"text": cb.Get("text").String() + cb.Get("thinking").String(),
			})
		case "content_block_delta":
			idx := int(ev.data.Get("index").Int())
			require.Less(t, idx, len(blocks))
			d := ev.data.Get("delta")
			blocks[idx]["text"] += d.Get("text").String() + d.Get("thinking").String() + d.Get("partial_json").String()
		}
	}
	return blocks
}

func chunk(delta, finish string) string {
	fr := "null"
	if finish != "" {
		fr = `"` + finish + `"`
	}
	return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"x","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + fr + `}]}` + "\n\n"
}

const keepAlive = ": keep-alive\n\n"

func TestLocalModel_StreamsReasoningContentAcrossKeepAliveComments(t *testing.T) {
	const id = "test-local-reasoning"
	upstream := newScriptedLocalUpstream(t,
		keepAlive+
			chunk(`{"role":"assistant","content":null,"reasoning_content":"Let me "}`, "")+
			keepAlive+
			chunk(`{"reasoning_content":"think."}`, "")+
			": keep-alive\r\n\r\n"+
			chunk(`{"content":"The answer"}`, "")+
			keepAlive+
			chunk(`{"content":" is 4."}`, "")+
			chunk(`{}`, "stop")+
			keepAlive+
			"data: [DONE]\n\n")
	svc, _, _ := localModelService(t, id, upstream)

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(localTestBody), rec, claudeCodeRequest(id)))

	events := parseClientSSE(t, rec.Body.String())
	blocks := contentBlocks(t, events)
	require.Len(t, blocks, 2, "one thinking block then one text block: %s", rec.Body.String())
	assert.Equal(t, "thinking", blocks[0]["type"])
	assert.Equal(t, "Let me think.", blocks[0]["text"])
	assert.Equal(t, "text", blocks[1]["type"])
	assert.Contains(t, blocks[1]["text"], "The answer is 4.")
	assert.NotContains(t, rec.Body.String(), "keep-alive", "upstream comment lines must not leak to the client")
	assert.Equal(t, "message_stop", events[len(events)-1].name)
}

// toolSearchBody mirrors Claude Code with tool search: a deferred tool is
// added mid-conversation by tool_reference and another by inline definition,
// one is removed, and the blocks ride on user, assistant and system turns.
const toolSearchBody = `{
	"model": "claude-sonnet-4-6",
	"max_tokens": 256,
	"stream": true,
	"system": [{"type": "text", "text": "You are Claude Code."}],
	"tools": [
		{"name": "Read", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}},
		{"name": "Edit", "description": "Edit a file", "input_schema": {"type": "object"}},
		{"name": "ToolSearch", "description": "Load a deferred tool", "input_schema": {"type": "object", "properties": {"query": {"type": "string"}}}},
		{"name": "mcp__db__query", "description": "Run SQL", "defer_loading": true, "input_schema": {"type": "object", "properties": {"sql": {"type": "string"}}}}
	],
	"messages": [
		{"role": "user", "content": "How many accounts are there?"},
		{"role": "assistant", "content": [{"type": "tool_use", "id": "toolu_1", "name": "ToolSearch", "input": {"query": "sql"}}]},
		{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": "loaded"},
			{"type": "tool_addition", "tool": {"type": "tool_reference", "name": "mcp__db__query"}},
			{"type": "text", "text": "continue"}
		]},
		{"role": "system", "content": [
			{"type": "tool_addition", "tool": {"name": "mcp__metrics__get", "description": "Fetch a metric", "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}}}},
			{"type": "tool_removal", "tool": {"type": "tool_reference", "name": "Edit"}},
			{"type": "text", "text": "Tool availability changed."}
		]},
		{"role": "assistant", "content": [
			{"type": "tool_removal", "toolset_id": "skills"},
			{"type": "text", "text": "Querying."}
		]},
		{"role": "user", "content": "go ahead"}
	]
}`

func TestLocalModel_ToolSearchBlocksBecomeValidOpenAIRequest(t *testing.T) {
	const id = "test-local-toolsearch"
	upstream := newLocalUpstream(t)
	svc, _, _ := localModelService(t, id, upstream)

	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(toolSearchBody), rec, claudeCodeRequest(id)))

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	require.Len(t, upstream.bodies, 1)
	sent := upstream.bodies[0]
	require.True(t, gjson.ValidBytes(sent))

	msgs := gjson.GetBytes(sent, "messages").Array()
	require.NotEmpty(t, msgs)
	for i, msg := range msgs {
		if i > 0 {
			assert.NotEqual(t, "system", msg.Get("role").String(), "no mid-history system entry: %s", msg.Raw)
		}
		if msg.Get("content").IsArray() {
			for _, part := range msg.Get("content").Array() {
				assert.Contains(t, []string{"text", "image_url"}, part.Get("type").String(), "only OpenAI content parts: %s", part.Raw)
			}
		}
	}
	for _, leaked := range []string{"tool_addition", "tool_removal", "tool_reference", "toolset_id", "defer_loading"} {
		assert.NotContains(t, string(sent), leaked)
	}
	assert.Contains(t, string(sent), "Tool availability changed.", "the system turn's text survives in place")

	tools := map[string]gjson.Result{}
	for _, tool := range gjson.GetBytes(sent, "tools").Array() {
		tools[tool.Get("function.name").String()] = tool
	}
	require.Contains(t, tools, "mcp__db__query", "a referenced deferred tool must be callable")
	assert.Equal(t, "string", tools["mcp__db__query"].Get("function.parameters.properties.sql.type").String())
	require.Contains(t, tools, "mcp__metrics__get", "an inline-defined added tool must be callable")
	assert.Equal(t, "Fetch a metric", tools["mcp__metrics__get"].Get("function.description").String())
	assert.Equal(t, "string", tools["mcp__metrics__get"].Get("function.parameters.properties.name.type").String())
	assert.NotContains(t, tools, "Edit", "a removed tool must not stay callable")
	assert.Contains(t, tools, "Read")
	assert.Contains(t, tools, "ToolSearch")
}

const toolTurnRequest = `{
	"model": "claude-sonnet-4-6",
	"max_tokens": 256,
	"stream": true,
	"tools": [{"name": "Read", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}],
	"messages": [{"role": "user", "content": "What is in go.mod?"}]
}`

func TestLocalModel_ToolUseRoundTripReturnsSecondTurn(t *testing.T) {
	const id = "test-local-tooltrip"
	upstream := newScriptedLocalUpstream(t,
		chunk(`{"role":"assistant","content":null,"reasoning_content":"Need the file."}`, "")+
			keepAlive+
			chunk(`{"tool_calls":[{"index":0,"id":"call_abc123","type":"function","function":{"name":"Read","arguments":""}}]}`, "")+
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}`, "")+
			chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"go.mod\"}"}}]}`, "")+
			chunk(`{}`, "tool_calls")+
			"data: [DONE]\n\n",
		chunk(`{"role":"assistant","content":"The module is weave-os/router."}`, "")+
			chunk(`{}`, "stop")+
			"data: [DONE]\n\n")
	svc, _, _ := localModelService(t, id, upstream)

	first := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(toolTurnRequest), first, claudeCodeRequest(id)))
	firstEvents := parseClientSSE(t, first.Body.String())
	blocks := contentBlocks(t, firstEvents)
	var toolUse map[string]string
	for _, b := range blocks {
		if b["type"] == "tool_use" {
			toolUse = b
		}
	}
	require.NotNil(t, toolUse, "turn one must surface the tool call: %s", first.Body.String())
	assert.Equal(t, "Read", toolUse["name"])
	assert.JSONEq(t, `{"path":"go.mod"}`, toolUse["text"])
	stop := ""
	for _, ev := range firstEvents {
		if ev.name == "message_delta" {
			stop = ev.data.Get("delta.stop_reason").String()
		}
	}
	assert.Equal(t, "tool_use", stop)

	// Claude Code echoes the assistant turn (thinking + tool_use) and answers it.
	second := `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 256,
		"stream": true,
		"tools": [{"name": "Read", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}],
		"messages": [
			{"role": "user", "content": "What is in go.mod?"},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "Need the file.", "signature": ""},
				{"type": "tool_use", "id": ` + jsonString(toolUse["id"]) + `, "name": "Read", "input": {"path": "go.mod"}}
			]},
			{"role": "user", "content": [{"type": "tool_result", "tool_use_id": ` + jsonString(toolUse["id"]) + `, "content": [{"type": "text", "text": "module weave-os/router"}]}]}
		]
	}`
	rec := httptest.NewRecorder()
	require.NoError(t, svc.ProxyMessages(routerKeyedCtx(), []byte(second), rec, claudeCodeRequest(id)))

	upstream.mu.Lock()
	require.Len(t, upstream.bodies, 2)
	sent := upstream.bodies[1]
	upstream.mu.Unlock()
	msgs := gjson.GetBytes(sent, "messages").Array()
	require.Len(t, msgs, 3, "user, assistant tool call, tool result: %s", sent)
	assert.Equal(t, "assistant", msgs[1].Get("role").String())
	callID := msgs[1].Get("tool_calls.0.id").String()
	require.NotEmpty(t, callID)
	assert.Equal(t, "Read", msgs[1].Get("tool_calls.0.function.name").String())
	assert.JSONEq(t, `{"path":"go.mod"}`, msgs[1].Get("tool_calls.0.function.arguments").String())
	assert.Equal(t, "tool", msgs[2].Get("role").String())
	assert.Equal(t, callID, msgs[2].Get("tool_call_id").String(), "the tool result must answer the call it belongs to")
	assert.Contains(t, msgs[2].Get("content").String(), "module weave-os/router")

	secondBlocks := contentBlocks(t, parseClientSSE(t, rec.Body.String()))
	var text string
	for _, b := range secondBlocks {
		if b["type"] == "text" {
			text += b["text"]
		}
	}
	assert.Contains(t, text, "The module is weave-os/router.")
}

func jsonString(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}
