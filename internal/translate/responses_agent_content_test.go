package translate_test

import (
	"testing"

	"weave-os/router/internal/translate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestUnwrapPlaintextAgentContent_RewritesPlaintextAsInputText(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":"go"},
		{"type":"agent_message","content":[
			{"type":"input_text","text":"Sender: /root/review"},
			{"type":"encrypted_content","encrypted_content":"I have not received <the> scope"}
		]}
	]}`)

	out, err := translate.UnwrapPlaintextAgentContentInResponsesInput(body)
	require.NoError(t, err)
	part := gjson.GetBytes(out, "input.1.content.1")
	assert.Equal(t, "input_text", part.Get("type").Str)
	assert.Equal(t, "I have not received <the> scope", part.Get("text").Str)
	assert.False(t, part.Get("encrypted_content").Exists())
	assert.Equal(t, "Sender: /root/review", gjson.GetBytes(out, "input.1.content.0.text").Str)
}

func TestUnwrapPlaintextAgentContent_KeepsOpenAICiphertext(t *testing.T) {
	body := []byte(`{"input":[{"type":"agent_message","content":[
		{"type":"encrypted_content","encrypted_content":"gAAAAABqwjURGyU8WDIxmYYmX72YWxzgPFXhbJ8au5m5"}
	]}]}`)

	out, err := translate.UnwrapPlaintextAgentContentInResponsesInput(body)
	require.NoError(t, err)
	assert.Equal(t, string(body), string(out))
}
