package core

// Per-model thinking levels, ported from oh-my-pi (github.com/can1357/oh-my-pi, MIT;
// see thinking_data.go for the exact source and copyright). The lookup, clamp and
// default rules mirror packages/catalog/src/model-thinking.ts:
//   - efforts: the model's own ordered ladder (thinking.efforts)
//   - default: thinking.defaultLevel, else the global default "high"
//     (coding-agent settings defaultThinkingLevel) clamped to the ladder
//   - clamp (clampThinkingLevelForModel): exact, else the highest supported level
//     below the request, else the lowest supported level.
// The ladder is then intersected with the levels CLIProxyAPI accepts for the model
// (cpaLevels) so the model(level) suffix never hits CPA's "level not supported" error.

import (
	"sort"
	"strings"
)

type ompRow struct {
	Efforts []string
	Default string
	Mode    string
}

// ThinkingProfile is the effort ladder used for one CPA model.
type ThinkingProfile struct {
	Efforts []string `json:"efforts"`
	Default string   `json:"default"`
	Mode    string   `json:"mode,omitempty"`
	Source  string   `json:"source"`
}

// Order is oh-my-pi's THINKING_EFFORTS order.
var Order = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

const ompGlobalDefault = "high"

// CPA provider key -> oh-my-pi providers to consult first.
var providerPref = map[string][]string{
	"antigravity":    {"google-antigravity"},
	"codex":          {"openai-codex", "openai"},
	"openai":         {"openai", "openai-codex"},
	"xai":            {"xai"},
	"commandcode-go": {"commandcode", "opencode-go"},
	"commandcode":    {"commandcode", "opencode-go"},
	"claude":         {"anthropic"},
	"gemini":         {"google"},
	"gemini-cli":     {"google"},
	"vertex":         {"google"},
	"aistudio":       {"google"},
	"kimi":           {"moonshot", "kimi-code"},
	"deepseek":       {"deepseek"},
}

var allOmp = []string{"google-antigravity", "openai-codex", "openai", "xai", "commandcode", "opencode-go", "anthropic", "google", "deepseek", "moonshot", "zai", "kimi-code", "minimax", "openrouter"}

// Levels CPA plugins declare for their own models (not in CPA's registry file).
// commandcode-go: models.go Thinking.Levels = none, auto, low, medium, high, max.
var pluginLevels = map[string][]string{
	"commandcode-go": {"low", "medium", "high", "max"},
}

func levelIdx(l string) int {
	for i, o := range Order {
		if o == l {
			return i
		}
	}
	return -1
}

// Clamp applies oh-my-pi's clampThinkingLevelForModel rule.
func Clamp(level string, ladder []string) string {
	if len(ladder) == 0 {
		return ""
	}
	level = strings.ToLower(strings.TrimSpace(level))
	for _, l := range ladder {
		if l == level {
			return l
		}
	}
	ri := levelIdx(level)
	if ri < 0 {
		return ""
	}
	out := ""
	for _, l := range ladder {
		if levelIdx(l) > ri {
			break
		}
		out = l
	}
	if out == "" {
		out = ladder[0]
	}
	return out
}

func base(id string) string {
	id = strings.ToLower(id)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	return id
}

// candidates yields the CPA id plus variants without effort-tier suffixes
// (antigravity "claude-sonnet-5-5-high", "claude-opus-4-6-thinking").
func candidates(model string) []string {
	b := base(model)
	out := []string{b}
	for _, suf := range []string{"-thinking", "-high", "-low", "-medium", "-max", "-xhigh", "-minimal"} {
		if strings.HasSuffix(b, suf) {
			out = append(out, strings.TrimSuffix(b, suf))
		}
	}
	return out
}

func noThinkingName(b string) bool {
	for _, k := range []string{"image", "tts", "video", "embedding", "imagine", "whisper", "audio"} {
		if strings.Contains(b, k) {
			return true
		}
	}
	return false
}

func ompExact(provs []string, cands []string) (ompRow, string, bool) {
	for _, c := range cands {
		for _, p := range provs {
			ids := make([]string, 0, len(ompThinking[p]))
			for id := range ompThinking[p] {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				if base(id) == c {
					return ompThinking[p][id], p + "/" + id, true
				}
			}
		}
	}
	return ompRow{}, "", false
}

// ompFamily finds the oh-my-pi id sharing the longest dash-segment prefix (>=2 segments).
func ompFamily(provs []string, c string) (ompRow, string, bool) {
	segs := strings.Split(c, "-")
	for n := len(segs) - 1; n >= 2; n-- {
		pre := strings.Join(segs[:n], "-")
		for _, p := range provs {
			ids := make([]string, 0)
			for id := range ompThinking[p] {
				b := base(id)
				if b == pre || strings.HasPrefix(b, pre+"-") {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				sort.Slice(ids, func(i, j int) bool {
					if len(ids[i]) != len(ids[j]) {
						return len(ids[i]) < len(ids[j])
					}
					return ids[i] < ids[j]
				})
				return ompThinking[p][ids[0]], p + "/" + ids[0], true
			}
		}
	}
	return ompRow{}, "", false
}

// cpaAccepted returns the levels CPA accepts (nil = unknown/any, empty = no thinking).
func cpaAccepted(model, provider string) ([]string, bool) {
	prov := strings.ToLower(provider)
	if l, ok := pluginLevels[prov]; ok {
		return l, true
	}
	if m, ok := cpaLevels[prov]; ok {
		if l, ok := m[model]; ok {
			if l == nil {
				return []string{}, true
			}
			if len(l) == 1 && l[0] == "*budget*" {
				return nil, true
			}
			return l, true
		}
	}
	return nil, false
}

// Profile returns the thinking ladder and default for a CPA model on a provider.
func Profile(model, provider string) ThinkingProfile {
	prov := strings.ToLower(strings.TrimSpace(provider))
	cands := candidates(model)
	pref := providerPref[prov]
	row, src, ok := ompExact(pref, cands)
	if !ok {
		row, src, ok = ompExact(allOmp, cands)
	}
	fam := false
	if !ok {
		for _, c := range cands {
			if row, src, ok = ompFamily(pref, c); ok {
				fam = true
				break
			}
		}
	}
	if !ok {
		for _, c := range cands {
			if row, src, ok = ompFamily(allOmp, c); ok {
				fam = true
				break
			}
		}
	}
	if fam && noThinkingName(cands[0]) {
		ok = false
	}
	tp := ThinkingProfile{Mode: row.Mode}
	acc, known := cpaAccepted(model, prov)
	switch {
	case ok:
		tp.Efforts = append([]string(nil), row.Efforts...)
		tp.Source = "oh-my-pi:" + src
		if fam {
			tp.Source = "oh-my-pi~" + src // family match
		}
	case noThinkingName(cands[0]):
		tp.Source = "none"
	case known && acc != nil:
		tp.Efforts = append([]string(nil), acc...)
		tp.Source = "cpa"
	default:
		tp.Efforts = []string{"low", "medium", "high"}
		tp.Source = "fallback"
	}
	if known && acc != nil && len(tp.Efforts) > 0 {
		if len(acc) == 0 {
			tp.Efforts, tp.Source = nil, tp.Source+"+cpa:no-thinking"
		} else {
			var in []string
			for _, e := range tp.Efforts {
				for _, a := range acc {
					if a == e {
						in = append(in, e)
					}
				}
			}
			if len(in) == 0 {
				in = append([]string(nil), acc...)
			}
			if len(in) != len(tp.Efforts) {
				tp.Source += "+cpa"
			}
			tp.Efforts = in
		}
	}
	if len(tp.Efforts) > 0 {
		def := row.Default
		if def == "" {
			def = ompGlobalDefault
		}
		tp.Default = Clamp(def, tp.Efforts)
	}
	return tp
}
