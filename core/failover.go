package core

import (
	"sort"
	"strings"
	"time"
)

// Failover defaults.
const (
	DefaultFailoverCooldownSeconds = 150
	DefaultFailoverMaxAttempts     = 3
)

// Hop is one failed upstream attempt of a request (failover history).
type Hop struct {
	At     time.Time `json:"at"`
	Model  string    `json:"model"`
	Status int       `json:"status,omitempty"`
	Error  string    `json:"error,omitempty"`
}

// Candidate is one model a request may be sent to, in failover order.
type Candidate struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Target   string `json:"target"` // model with effort suffix
	Tier     int    `json:"tier"`
}

// CoolView is a model currently skipped by routing after an upstream failure.
type CoolView struct {
	Model      string    `json:"model"`
	Until      time.Time `json:"until"`
	RemainingS int       `json:"remaining_s"`
	Status     int       `json:"status,omitempty"`
	Reason     string    `json:"reason,omitempty"`
}

type coolEntry struct {
	until  time.Time
	status int
	reason string
}

// Retriable reports whether an upstream failure should fail over to another model:
// transport errors / timeouts (status 0), 408, 429, 5xx (incl. auth_not_found 503),
// and provider-side 401/403/404 (credential missing, model not found). Request-caused
// 4xx (400, 413, 422, …) are returned to the client unchanged.
func Retriable(status int, errText string) bool {
	switch {
	case status == 0:
		return true
	case status == 408 || status == 429 || status >= 500:
		return true
	case status == 401 || status == 403 || status == 404:
		return true
	}
	e := strings.ToLower(errText)
	return strings.Contains(e, "auth_not_found") || strings.Contains(e, "timeout") || strings.Contains(e, "connection")
}

func baseModel(m string) string {
	b := strings.ToLower(strings.TrimSpace(m))
	if i := strings.Index(b, "("); i > 0 {
		b = b[:i]
	}
	return b
}

// Cool puts model into cooldown for d.
func (r *Router) Cool(model string, d time.Duration, status int, reason string) {
	if d <= 0 || model == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cool == nil {
		r.cool = map[string]coolEntry{}
	}
	r.cool[baseModel(model)] = coolEntry{until: r.Now().Add(d), status: status, reason: Clip(reason, 160, 0)}
}

// Uncool removes a model from cooldown (e.g. after a success).
func (r *Router) Uncool(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.cool, baseModel(model))
}

func (r *Router) coolingLocked(model string, now time.Time) bool {
	e, ok := r.cool[baseModel(model)]
	if !ok {
		return false
	}
	if !now.Before(e.until) {
		delete(r.cool, baseModel(model))
		return false
	}
	return true
}

// Cooling lists models in cooldown (expired entries auto-recover), soonest first.
func (r *Router) Cooling() []CoolView {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	out := []CoolView{}
	for m, e := range r.cool {
		if !now.Before(e.until) {
			delete(r.cool, m)
			continue
		}
		out = append(out, CoolView{Model: m, Until: e.until, RemainingS: int(e.until.Sub(now).Seconds() + 0.999), Status: e.status, Reason: e.reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until.Before(out[j].Until) })
	return out
}

// IsCooling reports whether model is in cooldown now.
func (r *Router) IsCooling(model string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.coolingLocked(model, r.Now())
}

// order returns eligible pool entries best-first: weighted ranking when scores exist,
// otherwise nearest tier at-or-above the target first, then lower tiers.
func order(cfg Config, tier int, scores []ModelScore, vm VirtualModel) []PoolEntry {
	var out []PoolEntry
	if len(scores) > 0 {
		for _, s := range scores {
			if p, ok := cfg.Entry(s.Model); ok {
				out = append(out, p)
			}
		}
		return out
	}
	for _, p := range cfg.Pool {
		if (vm.MinTier > 0 && p.Tier < vm.MinTier) || (vm.MaxTier > 0 && p.Tier > vm.MaxTier) {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		out = append(out, cfg.Pool...)
	}
	dist := func(t int) int {
		if t >= tier {
			return (t - tier) * 10
		}
		return (tier-t)*10 + 1
	}
	sort.SliceStable(out, func(i, j int) bool { return dist(out[i].Tier) < dist(out[j].Tier) })
	return out
}

// RunFailover tries cands in order, at most max attempts in total. try returns the
// upstream status (>=400 = failure) or an error. Non-retriable failures stop at once.
// onFail is called for each failed attempt. Returns the index of the candidate that
// produced the final outcome, its status and error.
func RunFailover(cands []Candidate, max int, try func(Candidate) (int, error), onFail func(Candidate, int, error)) (int, int, error) {
	if max <= 0 {
		max = DefaultFailoverMaxAttempts
	}
	last, status := -1, 0
	var err error
	for i, c := range cands {
		if i >= max {
			break
		}
		last = i
		status, err = try(c)
		if err == nil && status < 400 {
			return i, status, nil
		}
		if onFail != nil {
			onFail(c, status, err)
		}
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if !Retriable(status, msg) {
			break
		}
	}
	return last, status, err
}

// Target is the upstream model name with the effort raised by up escalation steps.
func (p PoolEntry) Target(up int) string {
	eff, _ := p.RaiseEffort(up)
	if eff == "" {
		return p.Model
	}
	return p.Model + "(" + eff + ")"
}

// AddHop records a failed attempt on the logged decision (matched by time+session)
// and, when next is set, switches the decision to that candidate.
func (r *Router) AddHop(at time.Time, session string, h Hop, next *Candidate) (Decision, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.log) - 1; i >= 0 && i >= len(r.log)-200; i-- {
		d := &r.log[i]
		if !d.At.Equal(at) || d.Session != session {
			continue
		}
		d.Hops = append(d.Hops, h)
		if next != nil {
			d.Model, d.Provider, d.TargetModel, d.ModelTier, d.Tier = next.Model, next.Provider, next.Target, next.Tier, next.Tier
			d.Signal = joinSig(d.Signal, "failover")
			if s := r.sessions[d.Session]; s != nil && s.decided.At.Equal(at) {
				s.decided = *d
			}
		}
		return *d, true
	}
	return Decision{}, false
}

// FailedUsage handles a failed upstream usage record for model: when failover is on
// and the failure is retriable, the model is put in cooldown so the next requests
// route around it. Returns true when it was treated as failover (callers then skip
// failure-escalation, since moving sideways is preferred over moving up a tier).
func (r *Router) FailedUsage(cfg Config, model string, status int, errText string) bool {
	if !cfg.Routing.Failover || !Retriable(status, errText) {
		return false
	}
	r.Cool(model, cfg.Routing.Cooldown(), status, errText)
	return true
}
