package core

import (
	"encoding/json"
	"regexp"
	"strings"
)

// HasImage reports whether a request body carries image input in any supported wire format:
// OpenAI chat image_url parts, Responses input_image, Claude image blocks (base64/url),
// Gemini inlineData/fileData with an image mime type, and bare data:image/... URLs.
func HasImage(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var v any
	if json.Unmarshal(body, &v) != nil {
		return strings.Contains(string(body), "data:image/")
	}
	return walkImage(v, 0)
}

func walkImage(v any, depth int) bool {
	if depth > 40 {
		return false
	}
	switch x := v.(type) {
	case map[string]any:
		if t, _ := x["type"].(string); t == "image_url" || t == "input_image" || t == "image" {
			return true
		}
		if _, ok := x["image_url"]; ok {
			return true
		}
		for _, k := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
			if m, ok := x[k].(map[string]any); ok {
				mt, _ := m["mimeType"].(string)
				if mt == "" {
					mt, _ = m["mime_type"].(string)
				}
				if mt == "" || strings.HasPrefix(strings.ToLower(mt), "image/") {
					return true
				}
			}
		}
		for k, c := range x {
			if k == "tools" || k == "functions" { // schemas may mention "image" types
				continue
			}
			if walkImage(c, depth+1) {
				return true
			}
		}
	case []any:
		for _, c := range x {
			if walkImage(c, depth+1) {
				return true
			}
		}
	case string:
		return strings.HasPrefix(x, "data:image/")
	}
	return false
}

// visionRe is a hand-maintained list of model families that accept image input
// (models.dev / vendor docs knowledge). Anything not matched is treated as text-only.
var visionRe = regexp.MustCompile(`(?i)(gpt-4o|gpt-4\.1|gpt-4\.5|gpt-5|gpt-6|\bo3\b|\bo4|claude|gemini|gemma-3|grok-4|grok-vision|kimi-k3|kimi-vl|glm-[\d.]+v|glm-4v|qwen[\w.-]*vl|qwen3?-omni|llama-4|pixtral|mistral-medium-3|vision|-vl\b|-vl-)`)
var textOnlyRe = regexp.MustCompile(`(?i)(deepseek|kimi-k2|-code\b|codex-mini|grok-code|embedding|whisper|tts)`)

// KnownImage guesses whether model accepts image input from its name.
func KnownImage(model string) bool {
	m := model
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if textOnlyRe.MatchString(m) {
		return false
	}
	return visionRe.MatchString(m)
}

// SupportsImage resolves the per-entry toggle, falling back to KnownImage.
func (p PoolEntry) SupportsImage() bool {
	if p.Image != nil {
		return *p.Image
	}
	return KnownImage(p.Model)
}

// ImageOnly returns a copy of c whose pool keeps only image-capable entries, plus the excluded models.
func (c Config) ImageOnly() (Config, []string) {
	out := c
	out.Pool = nil
	var dropped []string
	for _, p := range c.Pool {
		if p.SupportsImage() {
			out.Pool = append(out.Pool, p)
		} else {
			dropped = append(dropped, p.Model)
		}
	}
	return out, dropped
}
