// Package core holds the CPA-independent logic of the auto router:
// turn extraction, session stickiness, classification and decisions.
package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// Turn is what the router reads from one request body.
type Turn struct {
	HasImage      bool   // request carries image input (set by caller via HasImage)
	NewTurn       bool   // last message is a real user message (not a tool result)
	UserText      string // current user message text (stripped)
	PrevUserText  string // previous user message
	LastAssistant string // last assistant reply before the current user message
	UserCount     int    // number of real user messages so far
	FirstUser     string // first user message (session fallback key)
	ClientEffort  string // effort the client asked for, if any
	Token         string // opaque host callback id passed to the classifier
}

var injectedRe = regexp.MustCompile(`(?s)<(system-reminder|environment_context|user_instructions|ide_selection|ide_opened_file)>.*?</(system-reminder|environment_context|user_instructions|ide_selection|ide_opened_file)>`)

// Strip removes agent-injected blocks from user text.
func Strip(s string) string {
	return strings.TrimSpace(injectedRe.ReplaceAllString(s, ""))
}

type msg struct {
	role     string
	text     string
	toolOnly bool // only tool results
}

// ParseTurn understands OpenAI chat, OpenAI Responses, Anthropic Messages and Gemini bodies.
func ParseTurn(format string, body []byte) Turn {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return Turn{}
	}
	var msgs []msg
	t := Turn{}
	switch {
	case root["messages"] != nil:
		msgs = parseMessages(root["messages"])
		if e, ok := root["reasoning_effort"].(string); ok {
			t.ClientEffort = e
		}
		if th, ok := root["thinking"].(map[string]any); ok {
			if ty, _ := th["type"].(string); ty != "" {
				t.ClientEffort = "thinking:" + ty
			}
		}
	case root["input"] != nil:
		msgs = parseResponsesInput(root["input"])
		if r, ok := root["reasoning"].(map[string]any); ok {
			if e, ok := r["effort"].(string); ok {
				t.ClientEffort = e
			}
		}
	case root["contents"] != nil:
		msgs = parseGemini(root["contents"])
	}
	if len(msgs) == 0 {
		return t
	}
	lastUser := -1
	for i, m := range msgs {
		if m.role == "user" && !m.toolOnly {
			t.UserCount++
			if t.FirstUser == "" {
				t.FirstUser = m.text
			}
			if lastUser >= 0 {
				t.PrevUserText = msgs[lastUser].text
			}
			lastUser = i
		}
	}
	last := msgs[len(msgs)-1]
	t.NewTurn = last.role == "user" && !last.toolOnly
	if lastUser >= 0 {
		t.UserText = msgs[lastUser].text
		for i := lastUser - 1; i >= 0; i-- {
			if msgs[i].role == "assistant" && strings.TrimSpace(msgs[i].text) != "" {
				t.LastAssistant = msgs[i].text
				break
			}
		}
	}
	return t
}

func textOf(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return Strip(c), false
	case []any:
		var b strings.Builder
		toolOnly := len(c) > 0
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			ty, _ := pm["type"].(string)
			switch ty {
			case "tool_result", "function_call_output", "tool_use", "function_call":
				continue
			}
			toolOnly = false
			if s, ok := pm["text"].(string); ok {
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
		return Strip(b.String()), toolOnly
	}
	return "", false
}

func parseMessages(v any) []msg {
	arr, _ := v.([]any)
	var out []msg
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "tool" || role == "function" {
			out = append(out, msg{role: "user", toolOnly: true})
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		text, toolOnly := textOf(m["content"])
		if role == "user" && text == "" && !toolOnly {
			toolOnly = true // e.g. only injected reminders
		}
		out = append(out, msg{role: role, text: text, toolOnly: toolOnly})
	}
	return out
}

func parseResponsesInput(v any) []msg {
	if s, ok := v.(string); ok {
		return []msg{{role: "user", text: Strip(s)}}
	}
	arr, _ := v.([]any)
	var out []msg
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		ty, _ := m["type"].(string)
		role, _ := m["role"].(string)
		switch {
		case ty == "function_call_output" || ty == "custom_tool_call_output" || ty == "local_shell_call_output":
			out = append(out, msg{role: "user", toolOnly: true})
		case ty == "function_call" || ty == "reasoning" || ty == "custom_tool_call":
			out = append(out, msg{role: "assistant"})
		case role == "user" || role == "assistant":
			text, _ := textOfResponses(m["content"])
			if role == "user" && text == "" {
				out = append(out, msg{role: "user", toolOnly: true})
				continue
			}
			out = append(out, msg{role: role, text: text})
		}
	}
	return out
}

func textOfResponses(content any) (string, bool) {
	switch c := content.(type) {
	case string:
		return Strip(c), false
	case []any:
		var b strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if s, ok := pm["text"].(string); ok {
					b.WriteString(s)
					b.WriteString("\n")
				}
			}
		}
		return Strip(b.String()), false
	}
	return "", false
}

func parseGemini(v any) []msg {
	arr, _ := v.([]any)
	var out []msg
	for _, it := range arr {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "model" {
			role = "assistant"
		}
		parts, _ := m["parts"].([]any)
		var b strings.Builder
		toolOnly := len(parts) > 0
		for _, p := range parts {
			pm, _ := p.(map[string]any)
			if pm["functionResponse"] != nil || pm["functionCall"] != nil {
				continue
			}
			toolOnly = false
			if s, ok := pm["text"].(string); ok {
				b.WriteString(s)
			}
		}
		out = append(out, msg{role: role, text: Strip(b.String()), toolOnly: toolOnly && role == "user"})
	}
	return out
}

// SessionKey derives a conversation identity: explicit client session headers or
// body ids first, then a hash of the first user message.
func SessionKey(headers http.Header, body []byte, t Turn, scope string) (key, source string) {
	for _, h := range []string{"X-Claude-Code-Session-Id", "Session_id", "Session-Id", "X-Session-Id", "Conversation_id", "X-Opencode-Session"} {
		if v := strings.TrimSpace(headers.Get(h)); v != "" {
			return hashKey(scope + "|h|" + v), "header:" + strings.ToLower(h)
		}
	}
	var root map[string]any
	_ = json.Unmarshal(body, &root)
	if v, ok := root["prompt_cache_key"].(string); ok && v != "" {
		return hashKey(scope + "|pck|" + v), "body:prompt_cache_key"
	}
	if md, ok := root["metadata"].(map[string]any); ok {
		if v, ok := md["user_id"].(string); ok && v != "" {
			return hashKey(scope + "|uid|" + v), "body:metadata.user_id"
		}
	}
	if v, ok := root["previous_response_id"].(string); ok && v != "" {
		// Responses chains change id each turn; not stable, fall through.
		_ = v
	}
	return hashKey(scope + "|first|" + t.FirstUser), "first-user-hash"
}

func hashKey(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

// Clip keeps head and tail runes.
func Clip(s string, head, tail int) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	return string(r[:head]) + "\n…\n" + string(r[len(r)-tail:])
}
