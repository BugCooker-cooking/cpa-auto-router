package core

import (
	"testing"
	"time"
)

func TestHasImageFormats(t *testing.T) {
	cases := map[string]string{
		"openai-chat":     `{"model":"auto","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"https://x/y.png"}}]}]}`,
		"openai-chat-b64": `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}]}`,
		"responses":       `{"model":"auto","input":[{"role":"user","content":[{"type":"input_text","text":"hi"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}]}`,
		"claude":          `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},{"type":"text","text":"describe"}]}]}`,
		"claude-url":      `{"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://x/y.jpg"}}]}]}`,
		"gemini-inline":   `{"contents":[{"role":"user","parts":[{"text":"hi"},{"inlineData":{"mimeType":"image/jpeg","data":"AAAA"}}]}]}`,
		"gemini-snake":    `{"contents":[{"parts":[{"inline_data":{"mime_type":"image/png","data":"AAAA"}}]}]}`,
		"gemini-file":     `{"contents":[{"parts":[{"fileData":{"mimeType":"image/png","fileUri":"gs://b/x.png"}}]}]}`,
		"data-url-text":   `{"messages":[{"role":"user","content":"data:image/png;base64,AAAA"}]}`,
	}
	for name, b := range cases {
		if !HasImage([]byte(b)) {
			t.Errorf("%s: image not detected", name)
		}
	}
	neg := map[string]string{
		"text":       `{"messages":[{"role":"user","content":"draw me an image of a cat"}]}`,
		"gemini-pdf": `{"contents":[{"parts":[{"inlineData":{"mimeType":"application/pdf","data":"AAAA"}}]}]}`,
		"tools":      `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"x","parameters":{"type":"object","properties":{"type":{"enum":["image"]}}}}}]}`,
		"responses":  `{"input":[{"role":"user","content":[{"type":"input_text","text":"image_url please"}]}]}`,
	}
	for name, b := range neg {
		if HasImage([]byte(b)) {
			t.Errorf("%s: false positive", name)
		}
	}
}

func TestKnownImage(t *testing.T) {
	yes := []string{"gpt-5.6-sol", "claude-opus-5-5-high", "gemini-3.5-flash-lite", "grok-4.7", "commandcode-go/kimi-k3"}
	no := []string{"commandcode-go/deepseek-v4-flash", "commandcode-go/glm-5.3", "commandcode-go/kimi-k2.7-code", "grok-4.7-build-fast-code"}
	for _, m := range yes {
		if !KnownImage(m) {
			t.Errorf("%s should support images", m)
		}
	}
	for _, m := range no {
		if KnownImage(m) {
			t.Errorf("%s should be text-only", m)
		}
	}
}

func TestImageRouting(t *testing.T) {
	f := false
	cfg := DefaultConfig()
	cfg.Pool = []PoolEntry{{Tier: 1, Model: "commandcode-go/deepseek-v4-flash", Provider: "p"}, {Tier: 3, Model: "gpt-5.6-sol", Provider: "p"}, {Tier: 1, Model: "gemini-3.5-flash-lite", Provider: "p", Image: &f}}
	cfg.Classifier.Kind = "none"
	cfg.Normalize()
	r := NewRouter(func(string, time.Duration, string) (string, error) { return `{"tier":1,"w":{"speed":1}}`, nil })
	d := r.Decide(cfg, "auto", "openai", false, "s1", "t", Turn{NewTurn: true, UserCount: 1, UserText: "what is this", HasImage: true})
	if d.Model != "gpt-5.6-sol" || !d.HasImage || len(d.ImageFilter) != 2 {
		t.Fatalf("image route: %+v", d)
	}
	cfg.Pool = cfg.Pool[:1]
	d = r.Decide(cfg, "auto", "openai", false, "s2", "t", Turn{NewTurn: true, UserCount: 1, UserText: "x", HasImage: true})
	if !d.NoImageModel || d.Model != "" {
		t.Fatalf("expected no image model: %+v", d)
	}
}

func TestUsageTokensAttach(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Pool = []PoolEntry{{Tier: 1, Model: "gpt-5.6-sol", Provider: "p"}}
	cfg.Normalize()
	r := NewRouter(func(string, time.Duration, string) (string, error) { return `{"tier":1}`, nil })
	r.Decide(cfg, "auto", "openai", false, "s", "t", Turn{NewTurn: true, UserCount: 1, UserText: "hi"})
	c := int64(1024)
	if !r.ReportUsage("gpt-5.6-sol", Result{OK: true, Status: 200, InTokens: 2000, OutTokens: 50, CachedTokens: &c}) {
		t.Fatal("not attached")
	}
	d := r.Recent(1)[0]
	if d.Result == nil || d.Result.InTokens != 2000 || *d.Result.CachedTokens != 1024 || d.Result.OutTokens != 50 {
		t.Fatalf("%+v", d.Result)
	}
}
