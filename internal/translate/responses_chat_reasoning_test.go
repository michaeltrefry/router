package translate_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"weave-os/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func writeChatStream(t *testing.T, w *translate.ResponsesWriter, chunks ...string) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, chunk := range chunks {
		_, err := w.Write([]byte("data: " + chunk + "\n\n"))
		require.NoError(t, err)
	}
	_, err := w.Write([]byte("data: [DONE]\n\n"))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())
}

func doneItems(events []map[string]any) []map[string]any {
	var items []map[string]any
	for _, event := range events {
		if event["type"] == "response.output_item.done" {
			items = append(items, event["item"].(map[string]any))
		}
	}
	return items
}

func TestResponsesWriter_ChatReasoningStreamsAsReasoningItemBeforeText(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "local-model")
	w.SetBadgeText("badge")
	require.NoError(t, w.Prelude(true))
	writeChatStream(t, w,
		`{"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Think "},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"reasoning":"hard."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Answer."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	events := parseSSEEvents(t, rec.Body.Bytes())
	var summary string
	for _, event := range events {
		if event["type"] == "response.reasoning_summary_text.delta" {
			assert.EqualValues(t, 0, event["summary_index"])
			summary += event["delta"].(string)
		}
	}
	assert.Equal(t, "Think hard.", summary)

	items := doneItems(events)
	require.Len(t, items, 3, "badge message, reasoning, answer message")
	assert.Equal(t, "message", items[0]["type"])
	assert.Equal(t, "reasoning", items[1]["type"])
	assert.Equal(t, translate.RouterChatReasoningMarker, items[1]["encrypted_content"])
	assert.Equal(t, "Think hard.", items[1]["summary"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, "message", items[2]["type"])
	assert.Equal(t, "Answer.", items[2]["content"].([]any)[0].(map[string]any)["text"])

	// Output indexes never go backwards, and the completed envelope keeps them in order.
	var last float64 = -1
	for _, event := range events {
		if event["type"] != "response.output_item.added" {
			continue
		}
		idx := event["output_index"].(float64)
		assert.Greater(t, idx, last)
		last = idx
	}
	final := events[len(events)-1]
	require.Equal(t, "response.completed", final["type"])
	output := final["response"].(map[string]any)["output"].([]any)
	require.Len(t, output, 3)
	assert.Equal(t, "reasoning", output[1].(map[string]any)["type"])
}

func TestResponsesWriter_ThinkTagReasoningSplitsContent(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "")
	w.SetThinkTagReasoning(true)
	writeChatStream(t, w,
		`{"choices":[{"index":0,"delta":{"content":"<think>plan"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":" it</thi"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"nk>Done."},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	items := doneItems(parseSSEEvents(t, rec.Body.Bytes()))
	require.Len(t, items, 2, rec.Body.String())
	assert.Equal(t, "plan it", items[0]["summary"].([]any)[0].(map[string]any)["text"])
	assert.Equal(t, "Done.", items[1]["content"].([]any)[0].(map[string]any)["text"])
	assert.NotContains(t, rec.Body.String(), "<think>")
}

func TestResponsesWriter_NonStreamingChatReasoningLeadsOutput(t *testing.T) {
	rec := httptest.NewRecorder()
	w := translate.NewResponsesWriter(rec, "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, err := w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"Because.","content":"Yes."},"finish_reason":"stop"}]}`))
	require.NoError(t, err)
	require.NoError(t, w.Finalize())

	output := gjson.GetBytes(rec.Body.Bytes(), "output").Array()
	require.Len(t, output, 2)
	assert.Equal(t, "reasoning", output[0].Get("type").String())
	assert.Equal(t, "Because.", output[0].Get("summary.0.text").String())
	assert.Equal(t, "Yes.", output[1].Get("content.0.text").String())
}

func TestStripRouterReasoningFromResponsesInput(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-upstream"},` +
		`{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"local"}],"content":null,"encrypted_content":"` + translate.RouterChatReasoningMarker + `"},` +
		`{"type":"message","role":"user","content":"hi"}]}`)

	stripped, err := translate.StripRouterReasoningFromResponsesInput(body)
	require.NoError(t, err)
	input := gjson.GetBytes(stripped, "input").Array()
	require.Len(t, input, 2)
	assert.Equal(t, "rs_1", input[0].Get("id").String(), "upstream reasoning is untouched")
	assert.Equal(t, "message", input[1].Get("type").String())

	plain := []byte(`{"input":[{"type":"message","role":"user","content":"hi"}]}`)
	out, err := translate.StripRouterReasoningFromResponsesInput(plain)
	require.NoError(t, err)
	assert.Equal(t, plain, out)
}
