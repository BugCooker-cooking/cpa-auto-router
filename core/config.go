package core

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

// PoolEntry is one model in the auto pool.
type PoolEntry struct {
	Tier     int                `yaml:"tier" json:"tier"`
	Model    string             `yaml:"model" json:"model"`
	Provider string             `yaml:"provider" json:"provider"`
	Effort   string             `yaml:"effort" json:"effort"`                       // default effort, appended as model(effort)
	Efforts  []string           `yaml:"efforts,omitempty" json:"efforts,omitempty"` // allowed ladder override; empty = oh-my-pi profile
	Scores   map[string]float64 `yaml:"scores,omitempty" json:"scores,omitempty"`   // per-domain 0-10; empty = preset
	Image    *bool              `yaml:"image,omitempty" json:"image,omitempty"`     // accepts image input; nil = KnownImage(model)
}

// VirtualModel is a model name served by the router, optionally limited to a tier range.
type VirtualModel struct {
	Name    string `yaml:"name" json:"name"`
	MinTier int    `yaml:"min_tier,omitempty" json:"min_tier,omitempty"`
	MaxTier int    `yaml:"max_tier,omitempty" json:"max_tier,omitempty"`
}

// UnmarshalYAML accepts either "auto" or {name: auto, max_tier: 2}.
func (v *VirtualModel) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		v.Name = n.Value
		return nil
	}
	type plain VirtualModel
	return n.Decode((*plain)(v))
}

// UnmarshalJSON accepts either "auto" or an object.
func (v *VirtualModel) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		v.Name = s
		return nil
	}
	type plain VirtualModel
	return json.Unmarshal(b, (*plain)(v))
}

// JevConfig is the optional external classifier (OpenAI-compatible chat endpoint).
type JevConfig struct {
	BaseURL string `yaml:"base_url" json:"base_url"`
	Model   string `yaml:"model" json:"model"`
	APIKey  string `yaml:"api_key" json:"api_key"`
}

// ClassifierConfig picks how each new turn is classified.
type ClassifierConfig struct {
	Kind      string    `yaml:"kind" json:"kind"` // model | jev
	Model     string    `yaml:"model" json:"model"`
	Effort    string    `yaml:"effort" json:"effort"` // appended as model(effort) when set
	TimeoutMs int       `yaml:"timeout_ms" json:"timeout_ms"`
	Jev       JevConfig `yaml:"jev" json:"jev"`
}

// ReviewConfig is the optional review step (off by default).
type ReviewConfig struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	Markers []string `yaml:"markers" json:"markers"` // passive: markers in the last assistant reply
}

// EscalationConfig controls automatic tier bumps.
type EscalationConfig struct {
	Enabled          bool         `yaml:"enabled" json:"enabled"`
	OnUnhappy        bool         `yaml:"on_unhappy" json:"on_unhappy"`
	OnFailure        bool         `yaml:"on_failure" json:"on_failure"`
	MaxSteps         int          `yaml:"max_steps" json:"max_steps"`
	DecayAfterTurns  int          `yaml:"decay_after_turns" json:"decay_after_turns"`
	ResetIdleMinutes int          `yaml:"reset_idle_minutes" json:"reset_idle_minutes"`
	Review           ReviewConfig `yaml:"review" json:"review"`
}

// Config is the plugin config under plugins.configs.cpa-auto-router (and the saved panel state).
type Config struct {
	Enabled             bool             `yaml:"enabled" json:"enabled"`
	VirtualModels       []VirtualModel   `yaml:"virtual_models" json:"virtual_models"`
	Classifier          ClassifierConfig `yaml:"classifier" json:"classifier"`
	DefaultTier         int              `yaml:"default_tier" json:"default_tier"`
	RespectClientEffort bool             `yaml:"respect_client_effort" json:"respect_client_effort"`
	Pool                []PoolEntry      `yaml:"pool" json:"pool"`
	Routing             RoutingConfig    `yaml:"routing" json:"routing"`
	Escalation          EscalationConfig `yaml:"escalation" json:"escalation"`
	StateFile           string           `yaml:"state_file" json:"-"`

	// M0 flat keys, still accepted.
	LegacyClassifierModel string `yaml:"classifier_model" json:"-"`
	LegacyTimeoutMs       int    `yaml:"classifier_timeout_ms" json:"-"`
}

// DefaultConfig returns defaults matching DESIGN.md.
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		VirtualModels: []VirtualModel{{Name: "auto"}, {Name: "auto-fast", MaxTier: 2}, {Name: "auto-max", MinTier: 4}},
		Classifier:    ClassifierConfig{Kind: "model", Model: "gemini-3.8-flash-high", TimeoutMs: 8000},
		DefaultTier:   3,
		Routing:       RoutingConfig{Mode: "weighted", UnderPenalty: 1.5, OverPenalty: 1.0, StickyMargin: DefaultStickyMargin, Failover: true, FailoverCooldownSeconds: DefaultFailoverCooldownSeconds, FailoverMaxAttempts: DefaultFailoverMaxAttempts},
		Escalation: EscalationConfig{Enabled: true, OnUnhappy: true, OnFailure: true, MaxSteps: 2, DecayAfterTurns: 3, ResetIdleMinutes: 30,
			Review: ReviewConfig{Enabled: false, Markers: []string{"REVIEW: FAIL", "不符合预期"}}},
		StateFile: "plugins/cpa-auto-router.state.json",
	}
}

// Normalize fills defaults and folds legacy keys.
func (c *Config) Normalize() {
	d := DefaultConfig()
	if c.LegacyClassifierModel != "" {
		c.Classifier.Model = c.LegacyClassifierModel
		c.LegacyClassifierModel = ""
	}
	if c.LegacyTimeoutMs > 0 {
		c.Classifier.TimeoutMs = c.LegacyTimeoutMs
		c.LegacyTimeoutMs = 0
	}
	if c.Routing.Mode == "" {
		c.Routing.Mode = "weighted"
	}
	if c.Routing.FailoverCooldownSeconds <= 0 {
		c.Routing.FailoverCooldownSeconds = DefaultFailoverCooldownSeconds
	}
	if c.Routing.FailoverMaxAttempts <= 0 {
		c.Routing.FailoverMaxAttempts = DefaultFailoverMaxAttempts
	}
	if c.Routing.StickyMargin <= 0 {
		c.Routing.StickyMargin = DefaultStickyMargin
	}
	for i := range c.Pool {
		if e := c.Pool[i].Effort; e != "" {
			c.Pool[i].Effort = Clamp(e, c.Pool[i].Ladder())
		}
		// Fill every domain (incl. ones added later, e.g. aesthetics/logic) from presets; keep saved values.
		c.Pool[i].Scores = c.Pool[i].ScoresFor()
		if c.Pool[i].Image == nil {
			v := KnownImage(c.Pool[i].Model)
			c.Pool[i].Image = &v
		}
	}
	if c.Classifier.Kind == "" {
		c.Classifier.Kind = "model"
	}
	if c.Classifier.Model == "" {
		c.Classifier.Model = d.Classifier.Model
	}
	if c.Classifier.TimeoutMs <= 0 {
		c.Classifier.TimeoutMs = d.Classifier.TimeoutMs
	}
	if len(c.VirtualModels) == 0 {
		c.VirtualModels = d.VirtualModels
	}
	if c.DefaultTier < 1 || c.DefaultTier > 5 {
		c.DefaultTier = 3
	}
	if c.Escalation.MaxSteps <= 0 {
		c.Escalation.MaxSteps = 2
	}
	if c.Escalation.DecayAfterTurns <= 0 {
		c.Escalation.DecayAfterTurns = 3
	}
	if c.Escalation.ResetIdleMinutes <= 0 {
		c.Escalation.ResetIdleMinutes = 30
	}
	if len(c.Escalation.Review.Markers) == 0 {
		c.Escalation.Review.Markers = d.Escalation.Review.Markers
	}
	if c.StateFile == "" {
		c.StateFile = d.StateFile
	}
}

// Virtual returns the virtual model definition for a name.
func (c Config) Virtual(model string) (VirtualModel, bool) {
	for _, v := range c.VirtualModels {
		if strings.EqualFold(strings.TrimSpace(v.Name), strings.TrimSpace(model)) {
			return v, true
		}
	}
	return VirtualModel{}, false
}

// IsVirtual reports whether model is served by the router.
func (c Config) IsVirtual(model string) bool { _, ok := c.Virtual(model); return ok }

// Pick returns the pool entry for a tier: exact, else nearest higher, else nearest lower.
func (c Config) Pick(tier int) (PoolEntry, bool) {
	best, found, bestDist := PoolEntry{}, false, 1<<30
	for _, p := range c.Pool {
		d := p.Tier - tier
		if d < 0 {
			d = -d*10 + 1
		} else {
			d *= 10
		}
		if d < bestDist {
			best, found, bestDist = p, true, d
		}
	}
	return best, found
}

// MaxPoolTier is the highest tier present in the pool.
func (c Config) MaxPoolTier() int {
	m := 0
	for _, p := range c.Pool {
		if p.Tier > m {
			m = p.Tier
		}
	}
	return m
}

// Ladder is the entry's allowed effort ladder: explicit efforts, else the oh-my-pi profile.
func (p PoolEntry) Ladder() []string {
	if len(p.Efforts) > 0 {
		return p.Efforts
	}
	return Profile(p.Model, p.Provider).Efforts
}

// RaiseEffort returns the effort `steps` levels above the entry default on the model's own ladder.
// An empty default effort starts from the model's oh-my-pi default level.
func (p PoolEntry) RaiseEffort(steps int) (string, int) {
	if steps <= 0 {
		return p.Effort, 0
	}
	ladder := p.Ladder()
	if len(ladder) == 0 {
		return p.Effort, 0
	}
	cur := p.Effort
	if cur == "" {
		cur = Profile(p.Model, p.Provider).Default
	}
	cur = Clamp(cur, ladder)
	idx := 0
	for i, e := range ladder {
		if e == cur {
			idx = i
		}
	}
	n := idx + steps
	if n >= len(ladder) {
		n = len(ladder) - 1
	}
	if n == idx {
		return p.Effort, 0
	}
	return ladder[n], n - idx
}
