package core

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRetriable(t *testing.T) {
	for _, s := range []int{0, 408, 429, 500, 502, 503, 504, 401, 403, 404} {
		if !Retriable(s, "") {
			t.Errorf("%d should be retriable", s)
		}
	}
	for _, s := range []int{400, 413, 422} {
		if Retriable(s, "") {
			t.Errorf("%d should not be retriable", s)
		}
	}
	if !Retriable(400, "auth_not_found: no auth") || !Retriable(400, "context deadline: timeout") {
		t.Error("text match")
	}
}

func TestRunFailover(t *testing.T) {
	c := []Candidate{{Model: "a"}, {Model: "b"}, {Model: "c"}, {Model: "d"}}
	var tried []string
	fails := 0
	i, st, err := RunFailover(c, 3, func(x Candidate) (int, error) {
		tried = append(tried, x.Model)
		if x.Model == "c" {
			return 200, nil
		}
		return 503, errors.New("down")
	}, func(Candidate, int, error) { fails++ })
	if i != 2 || st != 200 || err != nil || fails != 2 || len(tried) != 3 {
		t.Fatalf("i=%d st=%d err=%v tried=%v", i, st, err, tried)
	}
	tried = nil
	_, st, err = RunFailover(c, 3, func(x Candidate) (int, error) { tried = append(tried, x.Model); return 503, errors.New("x") }, nil)
	if len(tried) != 3 || st != 503 || err == nil {
		t.Fatalf("max attempts: %v %d", tried, st)
	}
	tried = nil
	_, st, _ = RunFailover(c, 3, func(x Candidate) (int, error) { tried = append(tried, x.Model); return 400, errors.New("bad") }, nil)
	if len(tried) != 1 || st != 400 {
		t.Fatalf("non-retriable must stop: %v", tried)
	}
}

func foCfg() Config {
	cfg := DefaultConfig()
	cfg.Routing.Mode = "tier"
	cfg.Pool = []PoolEntry{{Tier: 3, Model: "a", Provider: "p"}, {Tier: 3, Model: "b", Provider: "p"}, {Tier: 4, Model: "c", Provider: "p"}}
	return cfg
}

func foDecide(r *Router, cfg Config, s string) Decision {
	tt := ParseTurn("openai", []byte(`{"messages":[{"role":"user","content":"`+s+`"}]}`))
	k, _ := SessionKey(http.Header{}, nil, tt, s)
	return r.Decide(cfg, "auto", "openai", false, k, "", tt)
}

func TestCooldownSkipAndRecover(t *testing.T) {
	now := time.Now()
	r := NewRouter(func(string, time.Duration, string) (string, error) { return "3", nil })
	r.Now = func() time.Time { return now }
	cfg := foCfg()
	d := foDecide(r, cfg, "one")
	if d.Model != "a" || len(d.Fallbacks) < 1 || d.Fallbacks[0].Model != "b" {
		t.Fatalf("base %+v", d)
	}
	if !r.FailedUsage(cfg, "a", 503, "") {
		t.Fatal("503 should fail over")
	}
	if r.FailedUsage(cfg, "b", 400, "") {
		t.Fatal("400 must not fail over")
	}
	if !r.IsCooling("a") || len(r.Cooling()) != 1 {
		t.Fatal("a should cool")
	}
	d = foDecide(r, cfg, "two")
	if d.Model != "b" || len(d.Cooled) != 1 {
		t.Fatalf("should skip cooling a: %+v", d)
	}
	now = now.Add(151 * time.Second)
	if r.IsCooling("a") {
		t.Fatal("cooldown should expire")
	}
	if d = foDecide(r, cfg, "three"); d.Model != "a" {
		t.Fatalf("recovered a expected, got %s", d.Model)
	}
}

func TestAllCoolingStillTriesBest(t *testing.T) {
	r := NewRouter(func(string, time.Duration, string) (string, error) { return "3", nil })
	cfg := foCfg()
	for _, m := range []string{"a", "b", "c"} {
		r.Cool(m, time.Minute, 503, "")
	}
	d := foDecide(r, cfg, "x")
	if d.Model != "a" || !d.AllCooling {
		t.Fatalf("%+v", d)
	}
}

func TestFailoverDisabled(t *testing.T) {
	r := NewRouter(nil)
	cfg := foCfg()
	cfg.Routing.Failover = false
	if r.FailedUsage(cfg, "a", 503, "") || r.IsCooling("a") {
		t.Fatal("disabled")
	}
}

func TestAddHopSwitches(t *testing.T) {
	r := NewRouter(func(string, time.Duration, string) (string, error) { return "3", nil })
	cfg := foCfg()
	d := foDecide(r, cfg, "hop")
	nd, ok := r.AddHop(d.At, d.Session, Hop{Model: "a", Status: 503}, &d.Fallbacks[0])
	if !ok || nd.Model != "b" || len(nd.Hops) != 1 {
		t.Fatalf("%+v", nd)
	}
}

func TestFailoverConfigDefaults(t *testing.T) {
	c := DefaultConfig()
	if !c.Routing.Failover || c.Routing.Cooldown() != 150*time.Second || c.Routing.MaxAttempts() != 3 || c.Routing.FailoverInline {
		t.Fatalf("%+v", c.Routing)
	}
	var y Config = DefaultConfig()
	if err := yaml.Unmarshal([]byte("routing:\n  mode: tier\n"), &y); err != nil {
		t.Fatal(err)
	}
	y.Normalize()
	if !y.Routing.Failover || y.Routing.MaxAttempts() != 3 || y.Routing.Cooldown() != 150*time.Second {
		t.Fatalf("partial yaml lost defaults: %+v", y.Routing)
	}
}
