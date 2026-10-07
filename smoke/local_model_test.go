//go:build smoke

package smoke

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
)

// localModelID is the model in smoke/fixtures/local-models.yaml; the router
// registers it under its own provider, local_<id>.
const (
	localModelID       = "smoke-local"
	localModelProvider = "local_" + localModelID
)

// TestLocalModel drives a self-hosted OpenAI-compatible model on the Anthropic
// ingress. Its cassettes are authored rather than recorded (there is no local
// server to record from) in the shape such servers stream: reasoning_content
// deltas, ": keep-alive" comment lines and OpenAI tool_calls.
func TestLocalModel(t *testing.T) {
	if os.Getenv("SMOKE_PROXY_MODE") == "record" {
		t.Skip("local-model cassettes are authored; record mode has no local server to call")
	}

	t.Run("streamed tool turn", func(t *testing.T) {
		body := newRequest("smoke-local-stream").tokens(1024).streaming().
			text("Use the Bash tool to list files in the current directory. Call the tool; do not answer in prose.").
			build(t)
		r := callModel(t, body, localModelID)
		if r.status != http.StatusOK {
			t.Fatalf("stream: want 200, got %d; body: %s", r.status, truncate(r.body, 600))
		}
		assertStreamWellFormed(t, r)
		assertServedByModel(t, r, localModelID, localModelProvider)

		blocks := streamedBlockTypes(t, r.body)
		if !slices.Contains(blocks, "thinking") {
			t.Errorf("want a thinking block from reasoning_content, got blocks %v", blocks)
		}
		if !slices.Contains(blocks, "tool_use") {
			t.Errorf("want a tool_use block from tool_calls, got blocks %v", blocks)
		}
		if r.message.StopReason != "tool_use" {
			t.Errorf("want stop_reason tool_use, got %q", r.message.StopReason)
		}
		if bytes.Contains(r.body, []byte("keep-alive")) {
			t.Errorf("upstream keep-alive comment leaked into the client stream: %s", truncate(r.body, 600))
		}
	})

	t.Run("non-streamed turn", func(t *testing.T) {
		body := newRequest("smoke-local-basic").tokens(1024).
			text("Reply with exactly the word: ok").build(t)
		r := callModel(t, body, localModelID)

		requireOKMessage(t, r)
		assertServedByModel(t, r, localModelID, localModelProvider)
		var text string
		for _, block := range r.message.Content {
			if block.Type == "text" {
				text += block.Text
			}
		}
		if !strings.Contains(text, "ok") {
			t.Errorf("want the local model's answer in a text block, got content %+v", r.message.Content)
		}
	})
}

// streamedBlockTypes returns the content_block.type of every
// content_block_start event in an Anthropic SSE body, in order.
func streamedBlockTypes(t *testing.T, raw []byte) []string {
	t.Helper()
	var types []string
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		payload, ok := strings.CutPrefix(sc.Text(), "data:")
		if !ok {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(payload)), &event) == nil && event.Type == "content_block_start" {
			types = append(types, event.ContentBlock.Type)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stream: %v", err)
	}
	return types
}
