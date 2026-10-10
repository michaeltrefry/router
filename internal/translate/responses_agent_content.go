package translate

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// openAICiphertextPrefix opens every OpenAI encrypted_content value: a Fernet
// token's version byte 0x80 base64url-encodes to "gA", followed by the
// leading zero bytes of its timestamp.
const openAICiphertextPrefix = "gAAAAA"

// UnwrapPlaintextAgentContentInResponsesInput rewrites agent_message
// encrypted_content parts that hold plaintext into input_text parts. OpenAI
// encrypts a Codex sub-agent's send_message text; when a sub-agent was served
// on another upstream, Codex still files that text as encrypted_content, and
// OpenAI then fails every later turn of the parent trying to decrypt it.
func UnwrapPlaintextAgentContentInResponsesInput(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"agent_message"`)) {
		return body, nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, nil
	}
	out := body
	for i, item := range input.Array() {
		if item.Get("type").Str != "agent_message" {
			continue
		}
		for j, part := range item.Get("content").Array() {
			text := part.Get("encrypted_content")
			if part.Get("type").Str != "encrypted_content" || text.Type != gjson.String || strings.HasPrefix(text.Str, openAICiphertextPrefix) {
				continue
			}
			rewritten, err := json.Marshal(map[string]string{"type": "input_text", "text": text.Str})
			if err != nil {
				return nil, err
			}
			path := "input." + strconv.Itoa(i) + ".content." + strconv.Itoa(j)
			if out, err = replaceResponsesRawField(out, path, rewritten); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}
