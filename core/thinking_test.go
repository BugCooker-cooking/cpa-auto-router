package core

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

var prodModels = [][2]string{
	{"gemini-3.8-flash-high", "antigravity"}, {"grok-4.7-build-fast", "xai"}, {"gpt-6-astra", "codex"}, {"gpt-5.6-terra", "codex"},
	{"gpt-5.6-luna", "codex"}, {"grok-composer-2.5-fast", "xai"}, {"claude-opus-5-5-high", "antigravity"}, {"commandcode/z-ai/glm-5.3-flash", "commandcode"},
	{"gpt-6-sol", "codex"}, {"grok-tts", "xai"}, {"commandcode-go/deepseek-v4.1-flash", "commandcode-go"}, {"commandcode-go/deepseek-v4-flash", "commandcode-go"},
	{"commandcode-go/deepseek-v4-flash-vision-exp", "commandcode-go"}, {"claude-opus-4-6-thinking", "antigravity"}, {"claude-sonnet-4-6", "antigravity"},
	{"gemini-3.1-flash-image", "antigravity"}, {"commandcode-go/glm-5.3", "commandcode-go"}, {"gpt-6.1-terra", "codex"}, {"gpt-image-2.5-sunburst", "codex"},
	{"gemini-4-argon-high", "antigravity"}, {"gpt-image-2.5", "codex"}, {"commandcode-go/glm-5.3-flash", "commandcode-go"}, {"commandcode-go/kimi-k2.6", "commandcode-go"},
	{"gpt-6.1-sol", "codex"}, {"grok-4.7", "xai"}, {"grok-imagine-image-2.0", "xai"}, {"commandcode-go/kimi-k2.7-code", "commandcode-go"}, {"gemini-3.5-flash-lite", "antigravity"},
	{"grok-imagine-video", "xai"}, {"claude-sonnet-5-5-high", "antigravity"}, {"commandcode/deepseek/deepseek-v4.1-flash", "commandcode"}, {"commandcode-go/kimi-k3", "commandcode-go"},
	{"gpt-5.6-sol", "codex"}, {"gpt-image-1.5", "codex"}, {"grok-4.6", "xai"}, {"commandcode-go/deepseek-v4-flash-fast", "commandcode-go"}, {"gpt-6-luna", "codex"},
	{"codex-auto-review", "codex"}, {"gpt-image-2", "codex"}, {"gpt-image-2.5-flare", "codex"}, {"some-unknown-model", "openai-compatible-x"},
}

func TestProfilesTable(t *testing.T) {
	var b strings.Builder
	b.WriteString("| CPA model | provider | levels | default | source |\n|---|---|---|---|---|\n")
	for _, m := range prodModels {
		p := Profile(m[0], m[1])
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", m[0], m[1], strings.Join(p.Efforts, "/"), p.Default, p.Source)
	}
	if out := os.Getenv("PROFILE_TABLE"); out != "" {
		os.WriteFile(out, []byte(b.String()), 0o644)
	}
	chk := func(m, pr, want, def string) {
		p := Profile(m, pr)
		if strings.Join(p.Efforts, "/") != want || p.Default != def {
			t.Errorf("%s: got %v def %s, want %s def %s", m, p.Efforts, p.Default, want, def)
		}
	}
	chk("claude-sonnet-5-5-high", "antigravity", "low/medium/high", "high")
	chk("claude-opus-5-5-high", "antigravity", "low/medium/high", "high")
	chk("commandcode-go/deepseek-v4.1-flash", "commandcode-go", "low/high/max", "high")
	chk("gpt-6.1-sol", "codex", "low/medium/high/xhigh/max", "high")
	if p := Profile("grok-tts", "xai"); len(p.Efforts) != 0 {
		t.Errorf("tts has efforts %v", p.Efforts)
	}
}

func TestClampAndRaise(t *testing.T) {
	if Clamp("medium", []string{"low", "high", "max"}) != "low" || Clamp("xhigh", []string{"low", "high", "max"}) != "high" || Clamp("minimal", []string{"high", "max"}) != "high" {
		t.Fatal("clamp")
	}
	p := PoolEntry{Model: "commandcode-go/deepseek-v4.1-flash", Provider: "commandcode-go", Effort: "high"}
	if e, n := p.RaiseEffort(1); e != "max" || n != 1 {
		t.Fatalf("raise %s %d", e, n)
	}
	p = PoolEntry{Model: "claude-opus-5-5-high", Provider: "antigravity"}
	if e, n := p.RaiseEffort(2); e != "" || n != 0 { // already at its top (high)
		t.Fatalf("raise opus %s %d", e, n)
	}
	p = PoolEntry{Model: "gpt-6.1-sol", Provider: "codex", Effort: "high"}
	if e, _ := p.RaiseEffort(1); e != "xhigh" {
		t.Fatalf("raise gpt %s", e)
	}
}
