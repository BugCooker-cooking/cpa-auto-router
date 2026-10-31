package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Decision is one routing decision.
type Decision struct {
	At           time.Time          `json:"at"`
	Session      string             `json:"session"`
	SessionSrc   string             `json:"session_source"`
	Virtual      string             `json:"virtual"`
	Format       string             `json:"format"`
	Stream       bool               `json:"stream"`
	NewTurn      bool               `json:"new_turn"`
	Turn         int                `json:"turn"`
	BaseTier     int                `json:"base_tier"`
	Escalation   int                `json:"escalation"`
	Tier         int                `json:"tier"`
	Model        string             `json:"model"`
	Provider     string             `json:"provider"`
	Effort       string             `json:"effort,omitempty"`
	TargetModel  string             `json:"target_model"`
	Reason       string             `json:"reason"`
	Signal       string             `json:"signal,omitempty"`
	ClassifyMs   int64              `json:"classify_ms"`
	ClassifyRaw  string             `json:"classify_raw,omitempty"`
	Snippet      string             `json:"snippet,omitempty"`
	ClientEffort string             `json:"client_effort,omitempty"`
	Weights      map[string]float64 `json:"weights,omitempty"`
	Scores       []ModelScore       `json:"scores,omitempty"`
	HasImage     bool               `json:"has_image,omitempty"`
	ImageFilter  []string           `json:"image_filtered,omitempty"` // pool models excluded because they lack image input
	NoImageModel bool               `json:"no_image_model,omitempty"` // image request but no image-capable pool entry
	Result       *Result            `json:"result,omitempty"`         // upstream outcome, filled from usage records
}

// Result is the upstream outcome of a routed request.
type Result struct {
	OK                  bool   `json:"ok"`
	Status              int    `json:"status,omitempty"`
	LatencyMs           int64  `json:"latency_ms,omitempty"`
	TTFTMs              int64  `json:"ttft_ms,omitempty"`
	OutTokens           int64  `json:"out_tokens,omitempty"`
	InTokens            int64  `json:"in_tokens,omitempty"`
	ReasoningTokens     int64  `json:"reasoning_tokens,omitempty"`
	CachedTokens        *int64 `json:"cached_tokens,omitempty"`         // cache hit / cache read; nil = provider did not report
	CacheCreationTokens *int64 `json:"cache_creation_tokens,omitempty"` // Claude cache writes
}

// ReportUsage attaches an upstream outcome to the newest unresolved decision for model.
func (r *Router) ReportUsage(model string, res Result) bool {
	base := strings.ToLower(model)
	if i := strings.Index(base, "("); i > 0 {
		base = base[:i]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	for i := len(r.log) - 1; i >= 0; i-- {
		d := &r.log[i]
		if now.Sub(d.At) > 10*time.Minute {
			break
		}
		if d.Result == nil && strings.ToLower(d.Model) == base {
			rc := res
			d.Result = &rc
			return true
		}
	}
	return false
}

type session struct {
	baseTier   int
	esc        int
	clean      int
	turnKey    string
	last       time.Time
	pendingErr string
	decided    Decision
	weights    map[string]float64
}

// SessionView is the panel view of one conversation.
type SessionView struct {
	Session    string    `json:"session"`
	Source     string    `json:"source"`
	BaseTier   int       `json:"base_tier"`
	Escalation int       `json:"escalation"`
	CleanTurns int       `json:"clean_turns"`
	Target     string    `json:"target"`
	Turn       int       `json:"turn"`
	Reason     string    `json:"reason"`
	Last       time.Time `json:"last"`
}

// ClassifyFunc runs the classifier prompt and returns the raw answer.
type ClassifyFunc func(prompt string, timeout time.Duration, token string) (string, error)

// Router holds sessions, caches and the decision log.
type Router struct {
	mu       sync.Mutex
	sessions map[string]*session
	srcs     map[string]string
	cache    map[string]cacheEntry
	failedAt time.Time
	log      []Decision
	recent   map[string]recentRoute // base model -> last session routed there
	Classify ClassifyFunc
	Now      func() time.Time
}

type cacheEntry struct {
	raw string
	at  time.Time
}

type recentRoute struct {
	session string
	at      time.Time
}

func NewRouter(f ClassifyFunc) *Router {
	return &Router{sessions: map[string]*session{}, srcs: map[string]string{}, cache: map[string]cacheEntry{}, recent: map[string]recentRoute{}, Classify: f, Now: time.Now}
}

const (
	cacheKeep    = 10 * time.Minute
	restAfterErr = 30 * time.Second
	sessionKeep  = 2 * time.Hour
	logKeep      = 500
)

var (
	continueRe = regexp.MustCompile(`(?i)^\s*(continue|go on|keep going|proceed|ok(ay)?|继续|接着|继续吧|好的?|go)\s*[.!。！]*\s*$`)
	unhappyRe  = regexp.MustCompile(`(?i)(不对|不行|还是不|没解决|没有解决|错了|重新来|重来|不是我要的|没用|you'?re wrong|that'?s wrong|doesn'?t work|does not work|didn'?t work|still (fail|broken|wrong|not)|not what i (asked|wanted)|try again)`)
	refusalRe  = regexp.MustCompile(`(?i)(I can(no|')t help with|I'?m (not able|unable) to (help|assist|complete)|I cannot (assist|complete|do that)|无法(帮助|完成|协助)|我不能(帮|完成))`)
)

// Decide returns the decision for one request.
func (r *Router) Decide(cfg Config, virtual string, format string, stream bool, sessKey, sessSrc string, t Turn) Decision {
	now := r.Now()
	vm, _ := cfg.Virtual(virtual)
	var dropped []string
	if t.HasImage {
		cfg, dropped = cfg.ImageOnly()
	}
	d := Decision{HasImage: t.HasImage, ImageFilter: dropped, NoImageModel: t.HasImage && len(cfg.Pool) == 0, At: now, Session: sessKey, SessionSrc: sessSrc, Virtual: vm.Name, Format: format, Stream: stream, NewTurn: t.NewTurn, Turn: t.UserCount, ClientEffort: t.ClientEffort}
	d.Snippet = Clip(strings.ReplaceAll(t.UserText, "\n", " "), 80, 0)
	turnKey := fmt.Sprintf("%s|%d|%s", vm.Name, t.UserCount, hashKey(t.UserText))

	r.mu.Lock()
	s := r.sessions[sessKey]
	if s != nil && cfg.Escalation.ResetIdleMinutes > 0 && now.Sub(s.last) > time.Duration(cfg.Escalation.ResetIdleMinutes)*time.Minute && s.esc > 0 {
		s.esc, s.clean = 0, 0
		d.Signal = "idle-reset"
	}
	r.mu.Unlock()

	sameTurn := s != nil && s.turnKey == turnKey
	base, unhappyFlag := 0, false
	var weights map[string]float64
	if s != nil {
		weights = s.weights
	}
	switch {
	case sameTurn:
		base, d.Reason = s.baseTier, "held-for-turn"
		if !t.NewTurn {
			d.Reason = "held-for-turn(tool-loop)"
		}
	case s != nil && continueRe.MatchString(t.UserText):
		base, d.Reason = s.baseTier, "continue-follows-previous"
	case !t.NewTurn && s != nil:
		base, d.Reason = s.baseTier, "follow-up-uses-session"
	default:
		var raw string
		base, d.Reason, d.ClassifyMs, raw = r.classify(cfg, t, s)
		d.ClassifyRaw = raw
		cl := ParseClassification(raw)
		unhappyFlag = cl.Unhappy
		if cl.Weights != nil {
			weights = cl.Weights
		} else {
			weights = HeuristicWeights(t.UserText)
			d.Signal = joinSig(d.Signal, "weights:heuristic")
		}
	}
	if base < 1 || base > 5 {
		base = cfg.DefaultTier
	}
	if vm.MinTier > 0 && base < vm.MinTier {
		base = vm.MinTier
	}
	if vm.MaxTier > 0 && base > vm.MaxTier {
		base = vm.MaxTier
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if s == nil {
		s = &session{}
		r.sessions[sessKey] = s
	}
	r.srcs[sessKey] = sessSrc
	esc := cfg.Escalation
	// Escalation signals.
	signal := ""
	if esc.Enabled {
		if esc.OnFailure && s.pendingErr != "" {
			signal = "failure:" + s.pendingErr
		} else if !sameTurn && t.NewTurn && s.turnKey != "" {
			switch {
			case esc.OnFailure && refusalRe.MatchString(t.LastAssistant):
				signal = "refusal"
			case esc.OnUnhappy && t.LastAssistant != "" && (unhappyFlag || unhappyRe.MatchString(t.UserText)):
				signal = "unhappy"
			case esc.Review.Enabled && hasMarker(t.LastAssistant, esc.Review.Markers):
				signal = "review-fail"
			}
		}
	}
	s.pendingErr = ""
	if signal != "" && s.esc < esc.MaxSteps {
		s.esc++
		s.clean = 0
		d.Signal = joinSig(d.Signal, "escalate+1:"+signal)
	} else if signal != "" {
		d.Signal = joinSig(d.Signal, "capped:"+signal)
	} else if !sameTurn && t.NewTurn && s.turnKey != "" && s.esc > 0 {
		s.clean++
		if s.clean >= esc.DecayAfterTurns {
			s.esc--
			s.clean = 0
			d.Signal = joinSig(d.Signal, "decay-1")
		}
	}
	if !esc.Enabled {
		s.esc = 0
	}

	// Apply: tiers first, then effort at the top tier.
	top := cfg.MaxPoolTier()
	if vm.MaxTier > 0 && vm.MaxTier < top {
		top = vm.MaxTier
	}
	tier := base
	up := s.esc
	for up > 0 && tier < top {
		tier++
		up--
	}
	d.BaseTier, d.Escalation, d.Tier = base, s.esc, tier
	p, ok := cfg.Pick(tier)
	if cfg.Routing.Mode != "tier" && len(cfg.Pool) > 0 {
		need := base + s.esc
		d.Weights = weights
		d.Scores = cfg.Rank(weights, need, vm)
		if len(d.Scores) > 0 {
			p, ok = cfg.Entry(d.Scores[0].Model)
			d.Tier = p.Tier
		}
	}
	if ok {
		d.Model, d.Provider = p.Model, p.Provider
		eff, _ := p.RaiseEffort(up)
		if cfg.RespectClientEffort && s.esc == 0 && t.ClientEffort != "" && !strings.HasPrefix(t.ClientEffort, "thinking:") {
			eff = t.ClientEffort
			if l := p.Ladder(); len(l) > 0 {
				eff = Clamp(eff, l)
			}
		}
		d.Effort = eff
		d.TargetModel = p.Model
		if eff != "" {
			d.TargetModel = p.Model + "(" + eff + ")"
		}
		r.recent[strings.ToLower(p.Model)] = recentRoute{session: sessKey, at: now}
	}
	s.weights = weights
	s.baseTier, s.turnKey, s.last, s.decided = base, turnKey, now, d
	for k, v := range r.sessions {
		if now.Sub(v.last) > sessionKeep {
			delete(r.sessions, k)
			delete(r.srcs, k)
		}
	}
	r.log = append(r.log, d)
	if len(r.log) > logKeep {
		r.log = r.log[len(r.log)-logKeep:]
	}
	return d
}

// ReportFailure marks the session last routed to model as failed; the next request escalates.
func (r *Router) ReportFailure(model string, status int) bool {
	base := strings.ToLower(model)
	if i := strings.Index(base, "("); i > 0 {
		base = base[:i]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rr, ok := r.recent[base]
	if !ok || r.Now().Sub(rr.at) > 10*time.Minute {
		return false
	}
	if s := r.sessions[rr.session]; s != nil {
		s.pendingErr = strconv.Itoa(status)
		return true
	}
	return false
}

func joinSig(a, b string) string {
	if a == "" {
		return b
	}
	return a + "," + b
}

func hasMarker(s string, markers []string) bool {
	for _, m := range markers {
		if m != "" && strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func (r *Router) classify(cfg Config, t Turn, s *session) (int, string, int64, string) {
	key := hashKey(t.PrevUserText + "\x00" + t.LastAssistant + "\x00" + t.UserText + "\x00" + cfg.Classifier.Model + cfg.Classifier.Kind)
	now := r.Now()
	prevTier := cfg.DefaultTier
	if s != nil && s.baseTier > 0 {
		prevTier = s.baseTier
	}
	r.mu.Lock()
	if c, ok := r.cache[key]; ok && now.Sub(c.at) < cacheKeep {
		r.mu.Unlock()
		return ParseClassification(c.raw).Tier, "classifier(cached)", 0, c.raw
	}
	if now.Sub(r.failedAt) < restAfterErr {
		r.mu.Unlock()
		return prevTier, "classifier-resting:fallback-previous", 0, ""
	}
	r.mu.Unlock()
	if r.Classify == nil || strings.TrimSpace(t.UserText) == "" {
		return prevTier, "no-text:fallback-previous", 0, ""
	}
	start := time.Now()
	raw, err := r.Classify(BuildPrompt(t), time.Duration(cfg.Classifier.TimeoutMs)*time.Millisecond, t.Token)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		r.mu.Lock()
		r.failedAt = now
		r.mu.Unlock()
		return prevTier, "classifier-error:fallback-previous: " + Clip(err.Error(), 120, 0), ms, ""
	}
	raw = Clip(strings.Join(strings.Fields(raw), " "), 400, 0)
	tier := ParseClassification(raw).Tier
	if tier == 0 {
		return prevTier, "classifier-unparsable:fallback-previous", ms, raw
	}
	r.mu.Lock()
	r.cache[key] = cacheEntry{raw: raw, at: now}
	for k, v := range r.cache {
		if now.Sub(v.at) > cacheKeep {
			delete(r.cache, k)
		}
	}
	r.mu.Unlock()
	return tier, "classifier", ms, raw
}

var digitRe = regexp.MustCompile(`[1-5]`)

// ParseTier takes the first digit 1..5 in the answer.
func ParseTier(raw string) int {
	m := digitRe.FindString(raw)
	if m == "" {
		return 0
	}
	n, _ := strconv.Atoi(m)
	return n
}

// Recent returns the newest decisions first.
func (r *Router) Recent(n int) []Decision {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Decision, 0, n)
	for i := len(r.log) - 1; i >= 0 && len(out) < n; i-- {
		out = append(out, r.log[i])
	}
	return out
}

// Sessions returns live conversations, newest first.
func (r *Router) Sessions() []SessionView {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SessionView, 0, len(r.sessions))
	for k, s := range r.sessions {
		out = append(out, SessionView{Session: k, Source: r.srcs[k], BaseTier: s.baseTier, Escalation: s.esc, CleanTurns: s.clean, Target: s.decided.TargetModel, Turn: s.decided.Turn, Reason: strings.TrimSpace(s.decided.Reason + " " + s.decided.Signal), Last: s.last})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Last.After(out[j-1].Last); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Patch replaces the logged decision with the same time and session (used after host-side fallbacks).
func (r *Router) Patch(d Decision) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.log) - 1; i >= 0 && i >= len(r.log)-50; i-- {
		if r.log[i].At.Equal(d.At) && r.log[i].Session == d.Session {
			r.log[i] = d
			if s := r.sessions[d.Session]; s != nil {
				s.decided = d
			}
			return
		}
	}
}
