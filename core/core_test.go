package core

import (
	"fmt"
	"strings"
	"net/http"
	"testing"
	"time"
)

func TestParseTurnFormats(t *testing.T) {
	chat := `{"messages":[{"role":"user","content":"hi <system-reminder>x</system-reminder>"},{"role":"assistant","content":"hello"},{"role":"user","content":"fix bug"},{"role":"assistant","content":null,"tool_calls":[{}]},{"role":"tool","content":"ok"}]}`
	tt := ParseTurn("openai", []byte(chat))
	if tt.NewTurn || tt.UserText != "fix bug" || tt.UserCount != 2 || tt.FirstUser != "hi" || tt.LastAssistant != "hello" {
		t.Fatalf("chat %+v", tt)
	}
	claude := `{"messages":[{"role":"user","content":[{"type":"text","text":"a"}]},{"role":"assistant","content":[{"type":"tool_use"}]},{"role":"user","content":[{"type":"tool_result","content":"x"}]}]}`
	tt = ParseTurn("claude", []byte(claude))
	if tt.NewTurn || tt.UserText != "a" {
		t.Fatalf("claude %+v", tt)
	}
	resp := `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},{"type":"function_call"},{"type":"function_call_output"}],"reasoning":{"effort":"low"}}`
	tt = ParseTurn("openai-response", []byte(resp))
	if tt.NewTurn || tt.UserText != "q" || tt.ClientEffort != "low" {
		t.Fatalf("resp %+v", tt)
	}
}

func TestRouterHoldsTurn(t *testing.T) {
	calls := 0
	r := NewRouter(func(p string, _ time.Duration, _ string) (string, error) { calls++; return "4", nil })
	cfg := DefaultConfig()
	cfg.Routing.Mode = "tier"
	cfg.Pool = []PoolEntry{{Tier: 2, Model: "a", Provider: "p"}, {Tier: 4, Model: "b", Provider: "p", Effort: "high"}}
	tt := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"design x"}]}`))
	k, _ := SessionKey(http.Header{}, nil, tt, "s")
	d := r.Decide(cfg, "auto", "openai", false, k, "", tt)
	if d.TargetModel != "b(high)" || calls != 1 {
		t.Fatalf("%+v", d)
	}
	tt2 := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"design x"},{"role":"assistant","content":"call"},{"role":"tool","content":"r"}]}`))
	d = r.Decide(cfg, "auto", "openai", false, k, "", tt2)
	if calls != 1 || d.Tier != 4 {
		t.Fatalf("tool loop reclassified %+v", d)
	}
	tt3 := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"design x"},{"role":"assistant","content":"done"},{"role":"user","content":"继续"}]}`))
	d = r.Decide(cfg, "auto", "openai", false, k, "", tt3)
	if calls != 1 || d.Reason != "continue-follows-previous" {
		t.Fatalf("continue %+v", d)
	}
	if p, _ := cfg.Pick(3); p.Model != "b" {
		t.Fatal("pick up")
	}
}

func TestEscalation(t *testing.T) {
	ans := "2"
	r := NewRouter(func(p string, _ time.Duration, _ string) (string, error) { return ans, nil })
	cfg := DefaultConfig()
	cfg.Routing.Mode = "tier"
	cfg.Pool = []PoolEntry{{Tier: 2, Model: "a", Provider: "p", Effort: "low"}, {Tier: 3, Model: "b", Provider: "p", Effort: "medium"}, {Tier: 4, Model: "c", Provider: "p", Effort: "high"}}
	mk := func(msgs string) Turn { return ParseTurn("openai", []byte(`{"messages":[`+msgs+`]}`)) }
	d := r.Decide(cfg, "auto", "openai", false, "s", "", mk(`{"role":"user","content":"write fn"}`))
	if d.Tier != 2 {
		t.Fatalf("base %+v", d)
	}
	d = r.Decide(cfg, "auto", "openai", false, "s", "", mk(`{"role":"user","content":"write fn"},{"role":"assistant","content":"x"},{"role":"user","content":"不对，还是报错"}`))
	if d.Tier != 3 || d.Escalation != 1 {
		t.Fatalf("unhappy %+v", d)
	}
	ans = "2 U"
	d = r.Decide(cfg, "auto", "openai", false, "s", "", mk(`{"role":"user","content":"write fn"},{"role":"assistant","content":"x"},{"role":"user","content":"hmm"},{"role":"assistant","content":"y"},{"role":"user","content":"look again at it"}`))
	if d.Tier != 4 || d.Escalation != 2 {
		t.Fatalf("unhappy2 %+v", d)
	}
	// capped at 2; failure report then same
	r.ReportFailure("c(high)", 500)
	d = r.Decide(cfg, "auto", "openai", false, "s", "", mk(`{"role":"user","content":"write fn"},{"role":"assistant","content":"x"},{"role":"user","content":"hmm"},{"role":"assistant","content":"y"},{"role":"user","content":"look again at it"},{"role":"assistant","content":null},{"role":"tool","content":"r"}`))
	if d.Escalation != 2 || d.Signal == "" {
		t.Fatalf("cap %+v", d)
	}
	ans = "2"
	hist := `{"role":"user","content":"write fn"},{"role":"assistant","content":"x"}`
	for i := 0; i < 3; i++ {
		hist += fmt.Sprintf(`,{"role":"user","content":"next task %d"},{"role":"assistant","content":"ok"}`, i)
		d = r.Decide(cfg, "auto", "openai", false, "s", "", mk(strings.TrimSuffix(hist, `,{"role":"assistant","content":"ok"}`)))
	}
	if d.Escalation != 1 {
		t.Fatalf("decay %+v", d)
	}
	// top tier: effort raises
	cfg.Pool = cfg.Pool[:1]
	d = r.Decide(cfg, "auto", "openai", false, "s2", "", mk(`{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}`))
	r.ReportFailure("a", 429)
	d = r.Decide(cfg, "auto", "openai", false, "s2", "", mk(`{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}`))
	if d.TargetModel != "a(medium)" {
		t.Fatalf("effort %+v", d)
	}
	// auto-fast caps tier
	cfg = DefaultConfig()
	cfg.Routing.Mode = "tier"
	cfg.Pool = []PoolEntry{{Tier: 2, Model: "a", Provider: "p"}, {Tier: 4, Model: "c", Provider: "p"}}
	ans = "4"
	d = r.Decide(cfg, "auto-fast", "openai", false, "s3", "", mk(`{"role":"user","content":"zz"}`))
	if d.Model != "a" {
		t.Fatalf("fast %+v", d)
	}
}

func TestWeighted(t *testing.T) {
	ans := ""
	r := NewRouter(func(p string, _ time.Duration, _ string) (string, error) { return ans, nil })
	cfg := DefaultConfig()
	for _, m := range []string{"gemini-3.5-flash-lite:1", "commandcode-go/deepseek-v4-flash:2", "gpt-5.6-sol:3", "gpt-6.1-sol:4", "claude-sonnet-5-5-high:4", "claude-opus-5-5-high:5"} {
		i := strings.LastIndex(m, ":")
		cfg.Pool = append(cfg.Pool, PoolEntry{Model: m[:i], Tier: int(m[i+1] - '0'), Provider: "p"})
	}
	cfg.Normalize()
	mk := func(s string) Turn { return ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"`+s+`"}]}`)) }
	cases := []struct{ ans, want string }{
		{`{"tier":1,"w":{"speed":0.7,"general":0.3},"u":false}`, "gemini-3.5-flash-lite"},
		{`{"tier":4,"w":{"frontend":0.6,"design":0.4}}`, "claude-sonnet-5-5-high"},
		{`{"tier":4,"w":{"backend":0.8,"general":0.2}}`, "gpt-6.1-sol"},
		{"```json\n{\"tier\":5,\"w\":{\"backend\":1}}\n```", "claude-opus-5-5-high"},
	}
	for i, c := range cases {
		ans = c.ans
		d := r.Decide(cfg, "auto", "openai", false, fmt.Sprint("w", i), "", mk(fmt.Sprint("q", i)))
		if d.Model != c.want {
			t.Errorf("%d: got %s scores %+v", i, d.Model, d.Scores)
		}
	}
	if c := ParseClassification(`{"tier":2,"w":{"text":2,"general":2},"u":true}`); c.Weights["text"] != 0.5 || !c.Unhappy || c.Tier != 2 {
		t.Errorf("parse %+v", c)
	}
}

func TestEightDomainsAndMigration(t *testing.T) {
	if len(Domains) != 8 {
		t.Fatalf("domains %v", Domains)
	}
	c := DefaultConfig()
	c.Pool = []PoolEntry{{Tier: 4, Model: "claude-sonnet-5-5-high", Scores: map[string]float64{"design": 3, "text": 7, "backend": 9, "frontend": 9, "general": 8, "speed": 5}}}
	c.Normalize()
	s := c.Pool[0].Scores
	if s["design"] != 3 || s["aesthetics"] != 8 || s["logic"] != 8 {
		t.Fatalf("migration %v", s)
	}
	w := HeuristicWeights("证明这个算法的正确性，并把页面做得好看一点")
	if w["logic"] == 0 || w["aesthetics"] == 0 {
		t.Fatalf("heuristic %v", w)
	}
	cl := ParseClassification(`{"tier":4,"w":{"logic":0.6,"aesthetics":0.4}}`)
	if cl.Weights["logic"] != 0.6 {
		t.Fatalf("parse %v", cl.Weights)
	}
}
