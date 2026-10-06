package translate

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyAnthropicToolChanges folds Anthropic mid-conversation tool_addition /
// tool_removal blocks into the request-level tools list for targets that have
// no message-level equivalent (the emitters then drop the blocks themselves).
// A tool's last directive in history decides: removed tools leave `tools`, and
// an added tool given by inline definition rather than a tool_reference to an
// existing entry is appended. Order is otherwise preserved so the upstream's
// cached prefix survives. With maxTools > 0 and the list over that cap, added
// tools are kept ahead of others so the emitter's truncation cannot drop them.
// tool_choice is reconciled with the resulting list. unresolved counts added
// tools that have neither an existing entry nor an inline definition; they
// are dropped since there is no definition to send.
func applyAnthropicToolChanges(body []byte, maxTools int) (out []byte, unresolved int, err error) {
	added, removed, inline := anthropicToolChangeState(gjson.GetBytes(body, "messages"))
	if len(added) == 0 && len(removed) == 0 {
		return body, 0, nil
	}

	var kept []string
	var keptNames []string
	present := make(map[string]struct{})
	for _, tool := range gjson.GetBytes(body, "tools").Array() {
		name := tool.Get("name").String()
		if _, gone := removed[name]; gone {
			continue
		}
		present[name] = struct{}{}
		kept = append(kept, tool.Raw)
		keptNames = append(keptNames, name)
	}
	for _, name := range inline.order {
		if _, ok := present[name]; ok {
			continue
		}
		if _, ok := added[name]; !ok {
			continue
		}
		present[name] = struct{}{}
		kept = append(kept, inline.defs[name])
		keptNames = append(keptNames, name)
	}
	for name := range added {
		if _, ok := present[name]; !ok {
			unresolved++
		}
	}

	if len(kept) == 0 && !gjson.GetBytes(body, "tools").Exists() {
		return body, unresolved, nil
	}
	if maxTools > 0 && len(kept) > maxTools {
		budget := maxTools
		for _, name := range keptNames {
			if _, ok := added[name]; ok {
				budget--
			}
		}
		var capped, cappedNames []string
		for i, name := range keptNames {
			if _, ok := added[name]; !ok {
				if budget <= 0 {
					continue
				}
				budget--
			}
			capped = append(capped, kept[i])
			cappedNames = append(cappedNames, name)
		}
		kept, keptNames = capped, cappedNames
	}

	out, err = sjson.SetRawBytes(body, "tools", []byte("["+strings.Join(kept, ",")+"]"))
	if err != nil {
		return nil, 0, fmt.Errorf("apply tool changes: %w", err)
	}
	out, err = reconcileToolChoice(out, keptNames)
	if err != nil {
		return nil, 0, err
	}
	return out, unresolved, nil
}

// reconcileToolChoice keeps tool_choice valid against the post-change tools,
// since non-Anthropic upstreams reject a tool_choice with no tools or one
// naming an undeclared tool. With no tools left tool_choice is dropped; a
// named choice whose tool is gone becomes "any", keeping the must-call intent.
func reconcileToolChoice(body []byte, toolNames []string) ([]byte, error) {
	kind, name := anthropicToolChoice(body)
	if kind == toolChoiceAbsent {
		return body, nil
	}
	if len(toolNames) == 0 {
		out, err := sjson.DeleteBytes(body, "tool_choice")
		if err != nil {
			return nil, fmt.Errorf("drop tool_choice: %w", err)
		}
		return out, nil
	}
	if kind != toolChoiceNamed || slices.Contains(toolNames, name) {
		return body, nil
	}
	out, err := sjson.SetBytes(body, "tool_choice.type", "any")
	if err == nil {
		out, err = sjson.DeleteBytes(out, "tool_choice.name")
	}
	if err != nil {
		return nil, fmt.Errorf("downgrade tool_choice: %w", err)
	}
	return out, nil
}

// effectiveTools returns the request's tools with Anthropic tool-change
// blocks applied (uncapped), i.e. the set the model may actually call.
func (e *RequestEnvelope) effectiveTools() gjson.Result {
	body := e.body
	if e.format == FormatAnthropic {
		if applied, _, err := applyAnthropicToolChanges(body, 0); err == nil {
			body = applied
		}
	}
	return gjson.GetBytes(body, "tools")
}

type inlineToolDefs struct {
	order []string
	defs  map[string]string
}

// anthropicToolChangeState replays tool directives in history order and
// returns the tools whose final state is added or removed, plus any inline
// definitions carried by additions (latest wins).
func anthropicToolChangeState(msgs gjson.Result) (added, removed map[string]struct{}, inline inlineToolDefs) {
	added = make(map[string]struct{})
	removed = make(map[string]struct{})
	inline.defs = make(map[string]string)
	for _, msg := range msgs.Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, block := range content.Array() {
			kind := anthropicSystemOnlyContentBlock(block.Get("type").String())
			if kind != anthropicSystemOnlyContentToolAddition && kind != anthropicSystemOnlyContentToolRemoval {
				continue
			}
			tool := block.Get("tool")
			name := tool.Get("name").String()
			if name == "" {
				continue
			}
			if kind == anthropicSystemOnlyContentToolRemoval {
				delete(added, name)
				removed[name] = struct{}{}
				continue
			}
			delete(removed, name)
			added[name] = struct{}{}
			if tool.Get("type").String() == "tool_reference" || !tool.Get("input_schema").Exists() {
				continue
			}
			if _, seen := inline.defs[name]; !seen {
				inline.order = append(inline.order, name)
			}
			inline.defs[name] = tool.Raw
		}
	}
	return added, removed, inline
}
