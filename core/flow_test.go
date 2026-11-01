package core

import (
	"net/http"
	"testing"
	"time"
)

// Reproduces the 0.3.5 flow-chart bug: classifier says T4, the session is escalated
// one step, so the T5 model is invoked — but the chart grouped by base_tier (T4).
func TestFlowTierUsesInvokedModel(t *testing.T) {
	r := NewRouter(func(string, time.Duration, string) (string, error) { return "4", nil })
	cfg := DefaultConfig()
	cfg.Routing.Mode = "tier"
	cfg.Escalation.Enabled = true
	cfg.Pool = []PoolEntry{{Tier: 4, Model: "m4", Provider: "p"}, {Tier: 5, Model: "m5", Provider: "p"}}
	tt := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"hard task"}]}`))
	k, _ := SessionKey(http.Header{}, nil, tt, "s")
	r.mu.Lock()
	r.sessions[k] = &session{baseTier: 4, esc: 1, last: time.Now()}
	r.mu.Unlock()
	d := r.Decide(cfg, "auto", "openai", false, k, "", tt)
	if d.Model != "m5" {
		t.Fatalf("want m5 invoked, got %+v", d)
	}
	if d.BaseTier != 4 {
		t.Fatalf("base tier %d", d.BaseTier)
	}
	if FlowTier(d) != 5 || d.ModelTier != 5 {
		t.Fatalf("flow tier %d model tier %d, want 5", FlowTier(d), d.ModelTier)
	}
}

// Weighted mode may pick a higher-tier model than the classifier tier; lane follows the model.
func TestFlowTierWeightedPick(t *testing.T) {
	d := Decision{BaseTier: 4, Tier: 4, ModelTier: 5, Model: "m5"}
	if FlowTier(d) != 5 {
		t.Fatal(FlowTier(d))
	}
	if FlowTier(Decision{BaseTier: 3, Tier: 4}) != 4 {
		t.Fatal("fallback to tier")
	}
}

func TestActiveInFlight(t *testing.T) {
	r := NewRouter(func(string, time.Duration, string) (string, error) { return "5", nil })
	cfg := DefaultConfig()
	cfg.Routing.Mode = "tier"
	cfg.Pool = []PoolEntry{{Tier: 5, Model: "m5", Provider: "p"}}
	tt := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"x"}]}`))
	k, _ := SessionKey(http.Header{}, nil, tt, "s")
	d := r.Decide(cfg, "auto", "openai", true, k, "", tt)
	a := r.Active()
	if len(a) != 1 || a[0].Model != "m5" || a[0].Tier != 5 || a[0].Session != d.Session {
		t.Fatalf("active %+v", a)
	}
	r.ReportUsage("m5", Result{OK: false, Status: 500})
	if n := len(r.Active()); n != 0 {
		t.Fatalf("still active after error: %d", n)
	}
	d2 := r.Decide(cfg, "auto", "openai", true, k, "", tt)
	r.MarkDone(d2, Result{Status: 400})
	if n := len(r.Active()); n != 0 {
		t.Fatalf("still active after MarkDone: %d", n)
	}
	r.Now = func() time.Time { return time.Now().Add(ActiveWindow + time.Minute) }
	r.Decide(cfg, "auto", "openai", true, k, "", tt)
	r.Now = func() time.Time { return time.Now().Add(2*ActiveWindow + 3*time.Minute) }
	if n := len(r.Active()); n != 0 {
		t.Fatalf("stale request still active: %d", n)
	}
}

func stickyScores() []ModelScore {
	return []ModelScore{{Model: "top", Tier: 4, Final: 8.0}, {Model: "cur", Tier: 4, Final: 7.5}, {Model: "far", Tier: 3, Final: 6.0}}
}

func TestStickyPick(t *testing.T) {
	r := RoutingConfig{Sticky: true, StickyMargin: 0.8}
	if s, ok := r.StickyPick(stickyScores(), "cur", 4); !ok || s.Model != "cur" {
		t.Fatal("within margin should stick")
	}
	if _, ok := r.StickyPick(stickyScores(), "far", 3); ok {
		t.Fatal("outside margin must switch")
	}
	if _, ok := r.StickyPick(stickyScores(), "gone", 4); ok {
		t.Fatal("model not eligible (not in ranked list) must switch")
	}
	if _, ok := (RoutingConfig{StickyMargin: 5}).StickyPick(stickyScores(), "cur", 4); ok {
		t.Fatal("off by default")
	}
	if s, ok := (RoutingConfig{Sticky: true}).StickyPick(stickyScores(), "cur", 4); !ok || s.Model != "cur" {
		t.Fatal("zero margin uses default 0.8")
	}
	esc := []ModelScore{{Model: "t5", Tier: 5, Final: 8.0}, {Model: "t4", Tier: 4, Final: 7.9}}
	if _, ok := r.StickyPick(esc, "t4", 5); ok {
		t.Fatal("escalation to a higher tier must not be blocked by stickiness")
	}
	if DefaultConfig().Routing.Sticky {
		t.Fatal("sticky must default off")
	}
}

func TestStickyDecideAndImage(t *testing.T) {
	r := NewRouter(func(string, time.Duration, string) (string, error) { return "4", nil })
	cfg := DefaultConfig()
	cfg.Routing.Sticky = true
	yes, no := true, false
	cfg.Pool = []PoolEntry{
		{Tier: 4, Model: "a", Provider: "p", Image: &no, Scores: map[string]float64{"general": 8}},
		{Tier: 4, Model: "b", Provider: "p", Image: &yes, Scores: map[string]float64{"general": 7.6}},
	}
	tt := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	k, _ := SessionKey(http.Header{}, nil, tt, "s")
	r.mu.Lock()
	r.sessions[k] = &session{baseTier: 4, last: time.Now(), decided: Decision{Model: "b"}, weights: map[string]float64{"general": 1}, turnKey: "x"}
	r.mu.Unlock()
	d := r.Decide(cfg, "auto", "openai", false, k, "", tt)
	if d.Model != "b" || !d.Sticky || d.StickyTop != "a" {
		t.Fatalf("expected sticky b, got %s sticky=%v top=%s scores=%+v", d.Model, d.Sticky, d.StickyTop, d.Scores)
	}
	cfg.Routing.Sticky = false
	r.mu.Lock()
	r.sessions[k].decided = Decision{Model: "b"}
	r.sessions[k].turnKey = "y"
	r.mu.Unlock()
	d = r.Decide(cfg, "auto", "openai", false, k, "", tt)
	if d.Model != "a" || d.Sticky {
		t.Fatalf("sticky off should pick top a, got %s", d.Model)
	}
	// Image request: current model "a" lacks image input, so it is not eligible.
	cfg.Routing.Sticky = true
	cfg.Pool = append(cfg.Pool, PoolEntry{Tier: 4, Model: "c", Provider: "p", Image: &yes, Scores: map[string]float64{"general": 9}})
	r.mu.Lock()
	r.sessions[k].decided = Decision{Model: "a"}
	r.sessions[k].turnKey = "z"
	r.mu.Unlock()
	ti := tt
	ti.HasImage = true
	d = r.Decide(cfg, "auto", "openai", false, k, "", ti)
	if d.Model == "a" || d.Sticky {
		t.Fatalf("image-ineligible model kept: %+v", d.Model)
	}
}
