package core

import (
	"encoding/json"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Domains are the capability labels, in display order.
// design = UI/UX/product design; aesthetics = taste/visual & stylistic polish;
// text = reading comprehension + writing/output; logic = logical rigor of the output;
// backend/frontend = code; general = knowledge/analysis; speed = latency.
var Domains = []string{"design", "aesthetics", "text", "logic", "backend", "frontend", "general", "speed"}

// DomainNames are the Chinese labels used by the panel.
var DomainNames = map[string]string{"design": "设计", "aesthetics": "审美", "text": "文本", "logic": "逻辑严谨", "backend": "后端编程", "frontend": "前端编程", "general": "通用", "speed": "速度"}

// PresetScores are default per-domain scores (0-10) for known models.
// These are hand estimates, not benchmark results; edit them in the panel.
var PresetScores = map[string]map[string]float64{
	"gemini-3.5-flash-lite":                 {"design": 4, "aesthetics": 4, "text": 5, "logic": 4, "backend": 4, "frontend": 4, "general": 5, "speed": 10},
	"gemini-3.8-flash-high":                 {"design": 6, "aesthetics": 6, "text": 7, "logic": 6, "backend": 6, "frontend": 7, "general": 7, "speed": 8},
	"gemini-4-argon-high":                   {"design": 9, "aesthetics": 9, "text": 8, "logic": 8, "backend": 8, "frontend": 9, "general": 9, "speed": 4},
	"commandcode-go/deepseek-v4-flash":      {"design": 4, "aesthetics": 4, "text": 6, "logic": 7, "backend": 7, "frontend": 6, "general": 6, "speed": 8},
	"commandcode-go/deepseek-v4.1-flash":    {"design": 4, "aesthetics": 4, "text": 6, "logic": 7, "backend": 7, "frontend": 6, "general": 6, "speed": 8},
	"commandcode-go/deepseek-v4-flash-fast": {"design": 4, "aesthetics": 4, "text": 6, "logic": 6, "backend": 6, "frontend": 5, "general": 6, "speed": 9},
	"commandcode-go/glm-5.3":                {"design": 6, "aesthetics": 6, "text": 7, "logic": 7, "backend": 8, "frontend": 7, "general": 7, "speed": 6},
	"commandcode-go/glm-5.3-flash":          {"design": 4, "aesthetics": 4, "text": 6, "logic": 6, "backend": 6, "frontend": 6, "general": 6, "speed": 8},
	"commandcode-go/kimi-k3":                {"design": 6, "aesthetics": 7, "text": 8, "logic": 7, "backend": 8, "frontend": 7, "general": 8, "speed": 5},
	"commandcode-go/kimi-k2.7-code":         {"design": 5, "aesthetics": 5, "text": 6, "logic": 7, "backend": 8, "frontend": 7, "general": 6, "speed": 7},
	"gpt-5.6-sol":                           {"design": 6, "aesthetics": 6, "text": 7, "logic": 8, "backend": 8, "frontend": 7, "general": 8, "speed": 6},
	"gpt-6-sol":                             {"design": 7, "aesthetics": 7, "text": 8, "logic": 9, "backend": 9, "frontend": 8, "general": 9, "speed": 5},
	"gpt-6.1-sol":                           {"design": 7, "aesthetics": 7, "text": 8, "logic": 9, "backend": 9, "frontend": 8, "general": 9, "speed": 4},
	"claude-sonnet-5-5-high":                {"design": 8, "aesthetics": 8, "text": 8, "logic": 8, "backend": 9, "frontend": 9, "general": 8, "speed": 5},
	"claude-opus-5-5-high":                  {"design": 9, "aesthetics": 9, "text": 9, "logic": 9, "backend": 10, "frontend": 9, "general": 9, "speed": 2},
	"grok-4.7":                              {"design": 6, "aesthetics": 6, "text": 7, "logic": 8, "backend": 8, "frontend": 7, "general": 8, "speed": 6},
	"grok-4.7-build-fast":                   {"design": 5, "aesthetics": 5, "text": 6, "logic": 7, "backend": 7, "frontend": 7, "general": 6, "speed": 8},
}

// tierFallback scores a model with no preset from its tier.
func tierFallback(tier int) map[string]float64 {
	b := float64(3 + tier)
	out := map[string]float64{}
	for _, d := range Domains {
		out[d] = b
	}
	out["speed"] = float64(11 - 2*tier)
	return out
}

// ScoresFor returns the entry's scores, falling back to presets, then tier.
func (p PoolEntry) ScoresFor() map[string]float64 {
	out := tierFallback(p.Tier)
	if ps, ok := PresetScores[strings.ToLower(p.Model)]; ok {
		for k, v := range ps {
			out[k] = v
		}
	}
	for k, v := range p.Scores {
		out[k] = v
	}
	return out
}

// RoutingConfig controls weighted selection.
type RoutingConfig struct {
	Mode         string  `yaml:"mode" json:"mode"`                   // weighted | tier
	UnderPenalty float64 `yaml:"under_penalty" json:"under_penalty"` // per tier a model is below the needed tier
	OverPenalty  float64 `yaml:"over_penalty" json:"over_penalty"`   // per tier a model is above the needed tier
	// Sticky routing (cache-first): keep the session's current model while it is still
	// eligible and its final score is within StickyMargin of the best candidate.
	Sticky       bool    `yaml:"sticky" json:"sticky"`
	StickyMargin float64 `yaml:"sticky_margin" json:"sticky_margin"` // score points; default DefaultStickyMargin
	// Failover: a model whose upstream call fails with a retriable error is put in
	// cooldown and skipped by routing; with FailoverInline the same request is also
	// retried on the next eligible model (see docs for ABI limitations).
	Failover                bool `yaml:"failover" json:"failover"`                                   // default true
	FailoverCooldownSeconds int  `yaml:"failover_cooldown_seconds" json:"failover_cooldown_seconds"` // default 150
	FailoverMaxAttempts     int  `yaml:"failover_max_attempts" json:"failover_max_attempts"`         // total attempts per request, default 3
	FailoverInline          bool `yaml:"failover_inline" json:"failover_inline"`                     // experimental, default false
}

// Cooldown is the configured failover cooldown.
func (r RoutingConfig) Cooldown() time.Duration {
	s := r.FailoverCooldownSeconds
	if s <= 0 {
		s = DefaultFailoverCooldownSeconds
	}
	return time.Duration(s) * time.Second
}

// MaxAttempts is the configured failover attempt cap (1..5).
func (r RoutingConfig) MaxAttempts() int {
	n := r.FailoverMaxAttempts
	if n <= 0 {
		n = DefaultFailoverMaxAttempts
	}
	return min(n, 5)
}

// DefaultStickyMargin is below the default one-tier over-penalty (1.0), so stickiness
// never outweighs a full tier of mismatch, but absorbs small weight jitter between turns.
const DefaultStickyMargin = 0.8

// StickyPick returns the candidate to keep for a session whose current model is cur,
// or ok=false to use the top-ranked model. scores must be ranked (best first) and only
// contain eligible models (vm range, image capability already filtered). need is the
// required tier; a current model below it is not kept when a higher-tier model leads
// (so escalation still moves the session up).
func (r RoutingConfig) StickyPick(scores []ModelScore, cur string, need int) (ModelScore, bool) {
	if !r.Sticky || cur == "" || len(scores) < 2 {
		return ModelScore{}, false
	}
	top := scores[0]
	if strings.EqualFold(top.Model, cur) {
		return ModelScore{}, false
	}
	m := r.StickyMargin
	if m <= 0 {
		m = DefaultStickyMargin
	}
	for _, s := range scores[1:] {
		if !strings.EqualFold(s.Model, cur) {
			continue
		}
		if s.Tier < need && top.Tier > s.Tier {
			return ModelScore{}, false
		}
		if top.Final-s.Final <= m+1e-9 {
			return s, true
		}
		return ModelScore{}, false
	}
	return ModelScore{}, false
}

// ModelScore is one candidate's evaluation, for logs and the panel.
type ModelScore struct {
	Model    string  `json:"model"`
	Tier     int     `json:"tier"`
	Weighted float64 `json:"weighted"`
	Final    float64 `json:"final"`
}

// Classification is the parsed classifier answer.
type Classification struct {
	Tier    int
	Weights map[string]float64
	Unhappy bool
}

var objRe = regexp.MustCompile(`(?s)\{.*\}`)

// ParseClassification accepts {"tier":3,"w":{"backend":0.7,...},"u":false}; falls back to a digit.
func ParseClassification(raw string) Classification {
	c := Classification{}
	if m := objRe.FindString(raw); m != "" {
		var v struct {
			Tier    json.Number        `json:"tier"`
			W       map[string]float64 `json:"w"`
			Weights map[string]float64 `json:"weights"`
			U       any                `json:"u"`
		}
		if json.Unmarshal([]byte(m), &v) == nil {
			if n, err := v.Tier.Int64(); err == nil {
				c.Tier = int(n)
			}
			w := v.W
			if len(w) == 0 {
				w = v.Weights
			}
			c.Weights = NormalizeWeights(w)
			switch u := v.U.(type) {
			case bool:
				c.Unhappy = u
			case string:
				c.Unhappy = strings.EqualFold(u, "true") || strings.EqualFold(u, "u")
			}
		}
	}
	if c.Tier < 1 || c.Tier > 5 {
		c.Tier = ParseTier(strings.Split(raw, "{")[0])
		if c.Tier == 0 {
			c.Tier = ParseTier(raw)
		}
	}
	if !c.Unhappy && c.Weights == nil && strings.Contains(strings.ToUpper(raw), "U") {
		c.Unhappy = true
	}
	return c
}

// NormalizeWeights keeps known domains and scales them to sum to 1.
func NormalizeWeights(w map[string]float64) map[string]float64 {
	out := map[string]float64{}
	sum := 0.0
	for _, d := range Domains {
		if v := w[d]; v > 0 {
			out[d] = v
			sum += v
		}
	}
	if sum == 0 {
		return nil
	}
	for k := range out {
		out[k] = math.Round(out[k]/sum*1000) / 1000
	}
	return out
}

// Rank scores every pool entry allowed by the virtual model's tier range.
// need is the effective tier (base + escalation); entries far below it are penalised more than entries above it.
func (c Config) Rank(w map[string]float64, need int, vm VirtualModel) []ModelScore {
	if len(w) == 0 {
		w = map[string]float64{"general": 1}
	}
	under, over := c.Routing.UnderPenalty, c.Routing.OverPenalty
	var out []ModelScore
	for _, p := range c.Pool {
		if (vm.MinTier > 0 && p.Tier < vm.MinTier) || (vm.MaxTier > 0 && p.Tier > vm.MaxTier) {
			continue
		}
		s := p.ScoresFor()
		ws := 0.0
		for d, x := range w {
			ws += x * s[d]
		}
		f := ws
		if p.Tier < need {
			f -= under * float64(need-p.Tier)
		} else {
			f -= over * float64(p.Tier-need)
		}
		out = append(out, ModelScore{Model: p.Model, Tier: p.Tier, Weighted: math.Round(ws*100) / 100, Final: math.Round(f*100) / 100})
	}
	speed := map[string]float64{}
	for _, p := range c.Pool {
		speed[p.Model] = p.ScoresFor()["speed"]
	}
	// Ties go to the faster (usually cheaper) model.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Final != out[j].Final {
			return out[i].Final > out[j].Final
		}
		return speed[out[i].Model] > speed[out[j].Model]
	})
	return out
}

// Entry returns the pool entry for model.
func (c Config) Entry(model string) (PoolEntry, bool) {
	for _, p := range c.Pool {
		if strings.EqualFold(p.Model, model) {
			return p, true
		}
	}
	return PoolEntry{}, false
}

var heur = []struct {
	d  string
	re *regexp.Regexp
}{
	{"frontend", regexp.MustCompile(`(?i)\b(react|vue|svelte|angular|css|tailwind|html|jsx|tsx|dom|component|landing page|前端|页面|样式)\b|前端|页面|样式`)},
	{"backend", regexp.MustCompile(`(?i)\b(api|rest|grpc|sql|postgres|mysql|redis|database|server|express|django|flask|go|golang|rust|java|backend|kafka|docker|kubernetes)\b|后端|数据库|接口`)},
	{"design", regexp.MustCompile(`(?i)\b(design|ui|ux|layout|typography|color|palette|wireframe|figma|visual)\b|设计|配色|排版|交互`)},
	{"aesthetics", regexp.MustCompile(`(?i)\b(beautiful|elegant|aesthetic|stylish|polish|pretty|look and feel|taste)\b|好看|美观|审美|高级感|质感|风格`)},
	{"logic", regexp.MustCompile(`(?i)\b(prove|proof|rigorous|logic|logical|edge cases?|invariant|correctness|verify|formal|algorithm|math)\b|证明|严谨|逻辑|推导|边界|正确性|算法`)},
	{"text", regexp.MustCompile(`(?i)\b(write|essay|copy|copywriting|translate|translation|summari[sz]e|email|blog|poem|read|explain|document|docs)\b|文案|翻译|写一|总结|润色|文章|阅读|理解|文档`)},
}

// HeuristicWeights guesses domain weights from keywords when the classifier gave none.
func HeuristicWeights(text string) map[string]float64 {
	w := map[string]float64{}
	for _, h := range heur {
		if n := len(h.re.FindAllString(text, 5)); n > 0 {
			w[h.d] = float64(n)
		}
	}
	if len(w) == 0 {
		w["general"] = 1
		if len([]rune(text)) < 60 {
			w["speed"] = 1
		}
	} else {
		w["general"] = 0.5
	}
	return NormalizeWeights(w)
}
