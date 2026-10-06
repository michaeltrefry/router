package translate

import (
	"bytes"
	"strings"

	"github.com/tidwall/gjson"
)

// RouterChatReasoningMarker fills encrypted_content on reasoning items the
// router synthesizes from a Chat Completions upstream's reasoning text. The
// item carries no reasoning any upstream can decrypt, so ingress drops it
// before conversion or native dispatch; a Responses client replays it like
// any other reasoning item.
const RouterChatReasoningMarker = "weave-router.chat-reasoning.v1"

// chatReasoningItem is one reasoning output item streamed as a single
// summary part.
type chatReasoningItem struct {
	itemID      string
	outputIndex int
	text        strings.Builder
	closed      bool
}

func (item *chatReasoningItem) output() map[string]any {
	return reasoningOutputItem(item.itemID, item.text.String())
}

func reasoningOutputItem(itemID, text string) map[string]any {
	return map[string]any{
		"id":                itemID,
		"type":              "reasoning",
		"summary":           []any{map[string]any{"type": "summary_text", "text": text}},
		"encrypted_content": RouterChatReasoningMarker,
	}
}

// chatDeltaReasoning returns the reasoning text of a Chat Completions delta
// or message: `reasoning_content` (llama.cpp, vLLM, DeepSeek) or `reasoning`
// (OpenRouter).
func chatDeltaReasoning(delta gjson.Result) string {
	if r := delta.Get("reasoning_content"); r.Type == gjson.String && r.Str != "" {
		return r.Str
	}
	if r := delta.Get("reasoning"); r.Type == gjson.String {
		return r.Str
	}
	return ""
}

// SetThinkTagReasoning reroutes a leading <think>…</think> block in the
// upstream's content channel into reasoning output, for models that stream
// chain-of-thought inline.
func (t *ResponsesWriter) SetThinkTagReasoning(on bool) {
	t.thinkTags = on
	t.splitter = thinkTagSplitter{}
}

// appendContent routes a content delta through the think-tag splitter when
// enabled, else straight to the assistant text item.
func (t *ResponsesWriter) appendContent(s string) error {
	if !t.thinkTags {
		t.hasUpstreamOutput = true
		return t.appendText(s)
	}
	return t.appendSegments(t.splitter.Feed(s))
}

func (t *ResponsesWriter) flushThinkTags() error {
	if !t.thinkTags {
		return nil
	}
	return t.appendSegments(t.splitter.Flush())
}

func (t *ResponsesWriter) appendSegments(segments []thinkSegment) error {
	for _, seg := range segments {
		if seg.kind == segThinking {
			if err := t.appendReasoning(seg.text); err != nil {
				return err
			}
			continue
		}
		t.hasUpstreamOutput = true
		if err := t.appendText(seg.text); err != nil {
			return err
		}
	}
	return nil
}

// appendReasoning streams reasoning text into the open reasoning item,
// opening one after closing any open assistant text item: a Responses
// client tracks one active item, so text deltas may not straddle it.
func (t *ResponsesWriter) appendReasoning(s string) error {
	if s == "" {
		return nil
	}
	item := t.openReasoningItem()
	if item == nil || item.closed {
		if err := t.closeTextItem(); err != nil {
			return err
		}
		item = &chatReasoningItem{itemID: newResponsesID("rs")}
		t.reasoningItems = append(t.reasoningItems, item)
		item.outputIndex = t.nextOutputIndex()
		if err := t.lifecycle.Output(item.outputIndex); err != nil {
			return err
		}
		if err := t.emitReasoningItemAdded(item); err != nil {
			return err
		}
	}
	item.text.WriteString(s)
	return t.writeEvent("response.reasoning_summary_text.delta", map[string]any{
		"item_id":       item.itemID,
		"output_index":  item.outputIndex,
		"summary_index": 0,
		"delta":         s,
	})
}

func (t *ResponsesWriter) openReasoningItem() *chatReasoningItem {
	if len(t.reasoningItems) == 0 {
		return nil
	}
	return t.reasoningItems[len(t.reasoningItems)-1]
}

// closeReasoningItem ends the open reasoning item, if any.
func (t *ResponsesWriter) closeReasoningItem() error {
	item := t.openReasoningItem()
	if item == nil || item.closed {
		return nil
	}
	item.closed = true
	text := item.text.String()
	part := map[string]any{"type": "summary_text", "text": text}
	if err := t.writeEvent("response.reasoning_summary_text.done", map[string]any{
		"item_id": item.itemID, "output_index": item.outputIndex, "summary_index": 0, "text": text,
	}); err != nil {
		return err
	}
	if err := t.writeEvent("response.reasoning_summary_part.done", map[string]any{
		"item_id": item.itemID, "output_index": item.outputIndex, "summary_index": 0, "part": part,
	}); err != nil {
		return err
	}
	return t.writeEvent("response.output_item.done", map[string]any{
		"output_index": item.outputIndex,
		"item":         item.output(),
	})
}

func (t *ResponsesWriter) emitReasoningItemAdded(item *chatReasoningItem) error {
	if err := t.writeEvent("response.output_item.added", map[string]any{
		"output_index": item.outputIndex,
		"item":         map[string]any{"id": item.itemID, "type": "reasoning", "summary": []any{}},
	}); err != nil {
		return err
	}
	return t.writeEvent("response.reasoning_summary_part.added", map[string]any{
		"item_id":       item.itemID,
		"output_index":  item.outputIndex,
		"summary_index": 0,
		"part":          map[string]any{"type": "summary_text", "text": ""},
	})
}

// StripRouterReasoningFromResponsesInput removes reasoning items the router
// synthesized from a Chat Completions upstream. They hold no reasoning any
// upstream can replay: kept, they would pin the turn to native Responses
// dispatch and then fail decryption there.
func StripRouterReasoningFromResponsesInput(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(RouterChatReasoningMarker)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	items := input.Array()
	kept := make([]string, 0, len(items))
	for _, item := range items {
		if item.Get("type").Str == "reasoning" && item.Get("encrypted_content").Str == RouterChatReasoningMarker {
			continue
		}
		kept = append(kept, item.Raw)
	}
	if len(kept) == len(items) {
		return body, nil
	}
	return replaceResponsesRawField(body, "input", []byte("["+strings.Join(kept, ",")+"]"))
}
