package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/BugCooker-cooking/cpa-auto-router/core"
	"gopkg.in/yaml.v3"
)

const pluginName = "cpa-auto-router"
const pluginVersion = "0.3.6"

//go:embed panel.html
var panelHTML []byte

var (
	currentConfig atomic.Value // core.Config (effective)
	yamlConfig    atomic.Value // core.Config from config.yaml
	router        = core.NewRouter(classify)
	lastProviders atomic.Value
	noImageMsg    atomic.Value // set when an image request has no image-capable target
	modelCache    atomic.Value // []availableModel from the last listModels
	saveMu        sync.Mutex
	stats         = &latStats{}
)

func handleMethod(method string, req []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if err := configure(req); err != nil {
			return nil, err
		}
		return okEnvelope(registration())
	case pluginabi.MethodModelRegister:
		return okEnvelope(modelRegistration())
	case pluginabi.MethodModelRoute:
		return routeModel(req)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginName})
	case pluginabi.MethodExecutorExecute, pluginabi.MethodExecutorExecuteStream:
		if out, handled := failoverExecute(method == pluginabi.MethodExecutorExecuteStream, req); handled {
			return out, nil
		}
		if m, _ := noImageMsg.Load().(string); m != "" {
			return errorEnvelopeStatus("invalid_request_error", m, 400), nil
		}
		return errorEnvelope("not_routed", "auto-router: request reached the placeholder executor; check that the router is enabled and the pool is not empty"), nil
	case pluginabi.MethodExecutorCountTokens, pluginabi.MethodExecutorHTTPRequest:
		if m, _ := noImageMsg.Load().(string); m != "" {
			return errorEnvelopeStatus("invalid_request_error", m, 400), nil
		}
		return errorEnvelope("not_routed", "auto-router: request reached the placeholder executor; check that the router is enabled and the pool is not empty"), nil
	case pluginabi.MethodUsageHandle:
		handleUsage(req)
		return okEnvelope(map[string]any{})
	case pluginabi.MethodManagementRegister:
		return okEnvelope(map[string]any{
			"routes": []map[string]string{
				{"Method": "GET", "Path": "/auto-router/decisions", "Description": "Recent auto-router decisions (JSON)"},
				{"Method": "GET", "Path": "/auto-router/state", "Description": "Auto-router config, sessions, decisions"},
				{"Method": "GET", "Path": "/auto-router/config", "Description": "Auto-router effective config"},
				{"Method": "PUT", "Path": "/auto-router/config", "Description": "Save auto-router config"},
				{"Method": "DELETE", "Path": "/auto-router/config", "Description": "Drop saved panel config (back to config.yaml)"},
				{"Method": "POST", "Path": "/auto-router/classifier-test", "Description": "Measure classifier latency"},
				{"Method": "GET", "Path": "/auto-router/models", "Description": "Models available in this CPA with provider and per-model thinking levels"},
			},
			"resources": []map[string]string{
				{"Path": "/panel", "Menu": "Auto Router", "Description": "自动模型路由：配置与状态"},
				{"Path": "/status", "Description": "Auto router status (same panel)"},
				{"Path": "/models.json", "Description": "Live CPA model list with per-model thinking levels (read-only, for the panel picker)"},
			},
		})
	case pluginabi.MethodManagementHandle:
		return handleManagement(req)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func configure(raw []byte) error {
	var req struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg := core.DefaultConfig()
	if len(req.ConfigYAML) > 0 {
		if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
			return err
		}
	}
	cfg.Normalize()
	yamlConfig.Store(cfg)
	eff := cfg
	if saved, ok := loadSaved(cfg.StateFile); ok {
		saved.StateFile = cfg.StateFile
		saved.Normalize()
		eff = saved
	}
	currentConfig.Store(eff)
	logModelsAtStartup()
	return nil
}

func loadSaved(path string) (core.Config, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return core.Config{}, false
	}
	cfg := core.DefaultConfig()
	if err := json.Unmarshal(b, &cfg); err != nil {
		return core.Config{}, false
	}
	return cfg, true
}

func loadedConfig() core.Config {
	if c, ok := currentConfig.Load().(core.Config); ok {
		return c
	}
	c := core.DefaultConfig()
	return c
}

func registration() map[string]any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "cpa-auto-router",
			GitHubRepository: "https://github.com/BugCooker-cooking/cpa-auto-router",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable auto routing."},
				{Name: "virtual_models", Type: pluginapi.ConfigFieldTypeArray, Description: "Virtual models: names or {name,min_tier,max_tier}."},
				{Name: "classifier", Type: pluginapi.ConfigFieldTypeObject, Description: "{kind: model|jev, model, effort, timeout_ms, jev:{base_url,model,api_key}}"},
				{Name: "default_tier", Type: pluginapi.ConfigFieldTypeInteger, Description: "Tier used when classification fails with no history."},
				{Name: "routing", Type: pluginapi.ConfigFieldTypeObject, Description: "{mode: weighted|tier, under_penalty, over_penalty, sticky (cache-first, default false), sticky_margin (default 0.8), failover (default true), failover_cooldown_seconds (150), failover_max_attempts (3), failover_inline (experimental, false)}."},
				{Name: "pool", Type: pluginapi.ConfigFieldTypeArray, Description: "List of {tier, model, provider, effort, efforts}."},
				{Name: "escalation", Type: pluginapi.ConfigFieldTypeObject, Description: "Auto escalation settings."},
				{Name: "state_file", Type: pluginapi.ConfigFieldTypeString, Description: "Where the panel saves its config (default plugins/cpa-auto-router.state.json)."},
			},
		},
		"capabilities": map[string]any{
			"model_registrar":      true,
			"model_router":         true,
			"executor":             true,
			"executor_model_scope": "static",
			"usage_plugin":         true,
			"management_api":       true,
		},
	}
}

func modelRegistration() pluginapi.ModelRegistrationResponse {
	cfg := loadedConfig()
	out := pluginapi.ModelRegistrationResponse{Provider: pluginName}
	for _, m := range cfg.VirtualModels {
		desc := "全池自动路由"
		if m.MaxTier > 0 || m.MinTier > 0 {
			desc = fmt.Sprintf("自动路由 T%d–T%d", max(1, m.MinTier), func() int {
				if m.MaxTier > 0 {
					return m.MaxTier
				}
				return 5
			}())
		}
		out.Models = append(out.Models, pluginapi.ModelInfo{
			ID: m.Name, Object: "model", OwnedBy: pluginName, DisplayName: m.Name, Description: desc,
			SupportedGenerationMethods: []string{"chat"},
		})
	}
	return out
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func routeModel(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	cfg := loadedConfig()
	if !cfg.Enabled || !cfg.IsVirtual(req.RequestedModel) || len(cfg.Pool) == 0 {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	lastProviders.Store(append([]string(nil), req.AvailableProviders...))
	t := core.ParseTurn(req.SourceFormat, req.Body)
	t.Token = req.HostCallbackID
	t.HasImage = core.HasImage(req.Body)
	scope := ""
	if v, ok := req.Metadata["caller_scope"].(string); ok {
		scope = v
	}
	key, src := core.SessionKey(req.Headers, req.Body, t, scope)
	d := router.Decide(cfg, req.RequestedModel, req.SourceFormat, req.Stream, key, src, t)
	if d.ClassifyMs > 0 && strings.HasPrefix(d.Reason, "classifier") {
		stats.add(cfg.Classifier.Kind+":"+cfg.Classifier.Model, d.ClassifyMs, !strings.Contains(d.Reason, "error"))
	}
	if d.NoImageModel {
		if m, ok := imageFallback(req.AvailableProviders); ok {
			d.Model, d.Provider, d.TargetModel, d.ModelTier = m.ID, m.Provider, m.ID, 0
			d.Reason += " → image-fallback(cpa)"
			router.Patch(d)
		} else {
			msg := "auto-router: 请求包含图片，但模型池与 CPA 中都没有支持图片输入的模型。请在 Auto Router 面板的模型池里为某个模型打开「支持图片输入」，或添加一个视觉模型。 (request contains images but no image-capable model is available)"
			noImageMsg.Store(msg)
			d.Reason += " → no-image-model(400)"
			router.Patch(d)
			router.MarkDone(d, core.Result{OK: false, Status: 400})
			hostLog("warn", "auto-router: image request with no image-capable model; returning 400")
			return okEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Target: pluginName, Reason: "auto-router:no-image-model"})
		}
	}
	noImageMsg.Store("")
	if rp, ok := resolveProvider(d.Provider, req.AvailableProviders); ok {
		d.Provider = rp
	} else {
		d.Reason += " (provider-unavailable)"
	}
	if d.Provider == "" || d.TargetModel == "" {
		router.MarkDone(d, core.Result{OK: false})
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	if cfg.Routing.Failover && cfg.Routing.FailoverInline && len(d.Fallbacks) > 0 {
		rememberInline(req.Body, d, req.AvailableProviders)
		hostLog("info", fmt.Sprintf("auto-router: %s session=%s -> %s/%s via inline failover executor (%d alternatives)", d.Virtual, d.Session, d.Provider, d.TargetModel, len(d.Fallbacks)))
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: true, TargetKind: pluginapi.ModelRouteTargetSelf, Target: pluginName, Reason: "auto-router:" + d.Reason + " (failover)"})
	}
	hostLog("info", fmt.Sprintf("auto-router: %s fmt=%s session=%s(%s) turn=%d tier=%d(base %d +%d) -> %s/%s reason=%s %s classify=%dms",
		d.Virtual, d.Format, d.Session, d.SessionSrc, d.Turn, d.Tier, d.BaseTier, d.Escalation, d.Provider, d.TargetModel, d.Reason, d.Signal, d.ClassifyMs))
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:     true,
		TargetKind:  pluginapi.ModelRouteTargetProvider,
		Target:      d.Provider,
		TargetModel: d.TargetModel,
		Reason:      "auto-router:" + d.Reason,
	})
}

// resolveProvider maps a configured provider name to a key the host accepts.
// openai-compatibility entries are registered as "openai-compatible-<name>".
func resolveProvider(name string, avail []string) (string, bool) {
	n := strings.ToLower(strings.TrimSpace(name))
	if len(avail) == 0 {
		return n, true
	}
	for _, cand := range []string{n, "openai-compatible-" + n, "openai-compatibility-" + n} {
		for _, a := range avail {
			if strings.EqualFold(a, cand) {
				return a, true
			}
		}
	}
	return n, false
}

func handleUsage(raw []byte) {
	var rec struct {
		Model   string
		Alias   string
		Failed  bool
		Latency time.Duration
		TTFT    time.Duration
		Failure struct{ StatusCode int }
		Detail  struct {
			InputTokens, OutputTokens, ReasoningTokens, CachedTokens, CacheReadTokens, CacheCreationTokens int64
		}
	}
	if json.Unmarshal(raw, &rec) != nil {
		return
	}
	cfg := loadedConfig()
	if strings.EqualFold(rec.Model, cfg.Classifier.Model) && !cfg.IsVirtual(rec.Alias) {
		return
	}
	res := core.Result{OK: !rec.Failed, Status: rec.Failure.StatusCode, LatencyMs: rec.Latency.Milliseconds(), TTFTMs: rec.TTFT.Milliseconds(), OutTokens: rec.Detail.OutputTokens, InTokens: rec.Detail.InputTokens, ReasoningTokens: rec.Detail.ReasoningTokens}
	if c := max(rec.Detail.CachedTokens, rec.Detail.CacheReadTokens); c > 0 {
		res.CachedTokens = &c
	}
	if c := rec.Detail.CacheCreationTokens; c > 0 {
		res.CacheCreationTokens = &c
	}
	if !rec.Failed {
		res.Status = 200
	}
	if cfg.IsVirtual(rec.Alias) || rec.Alias == "" {
		router.ReportUsage(rec.Model, res)
	}
	if !rec.Failed {
		router.Uncool(rec.Model)
		return
	}
	if router.FailedUsage(cfg, rec.Model, rec.Failure.StatusCode, "") {
		hostLog("info", fmt.Sprintf("auto-router: upstream failure %d on %s; cooling down %s, routing skips it", rec.Failure.StatusCode, rec.Model, cfg.Routing.Cooldown()))
		return
	}
	if router.ReportFailure(rec.Model, rec.Failure.StatusCode) {
		hostLog("info", fmt.Sprintf("auto-router: upstream failure %d on %s; next request in that session escalates", rec.Failure.StatusCode, rec.Model))
	}
}

// ---------- classifier ----------

type hostModelExecutionRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func classify(prompt string, timeout time.Duration, cb string) (string, error) {
	cfg := loadedConfig()
	return classifyWith(cfg.Classifier, prompt, timeout, cb)
}

func classifyWith(c core.ClassifierConfig, prompt string, timeout time.Duration, cb string) (string, error) {
	type result struct {
		s   string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var s string
		var err error
		if c.Kind == "jev" {
			s, err = classifyJev(c.Jev, prompt, timeout)
		} else {
			s, err = classifyHost(c, prompt, cb)
		}
		ch <- result{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err
	case <-time.After(timeout):
		return "", fmt.Errorf("classifier timeout after %s", timeout)
	}
}

func chatBody(model, prompt string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": 120,
		"stream":     false,
	})
	return b
}

func classifyHost(c core.ClassifierConfig, prompt, cb string) (string, error) {
	model := c.Model
	if c.Effort != "" {
		model += "(" + c.Effort + ")"
	}
	raw, err := callHost(pluginabi.MethodHostModelExecute, hostModelExecutionRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: "openai", ExitProtocol: "openai", Model: model,
			Body: chatBody(model, prompt), Headers: http.Header{"Content-Type": {"application/json"}},
		},
		HostCallbackID: cb,
	})
	if err != nil {
		return "", err
	}
	var resp pluginapi.HostModelExecutionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("classifier status %d: %s", resp.StatusCode, core.Clip(string(resp.Body), 200, 0))
	}
	return chatContent(resp.Body)
}

func chatContent(body []byte) (string, error) {
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || len(out.Choices) == 0 {
		return "", fmt.Errorf("classifier: unexpected body %s", core.Clip(string(body), 200, 0))
	}
	return out.Choices[0].Message.Content, nil
}

// classifyJev calls an OpenAI-compatible endpoint (Jev or any other) with the user's key.
func classifyJev(j core.JevConfig, prompt string, timeout time.Duration) (string, error) {
	if j.BaseURL == "" || j.APIKey == "" {
		return "", fmt.Errorf("jev classifier: base_url and api_key are required")
	}
	model := j.Model
	if model == "" {
		model = "jev"
	}
	url := strings.TrimRight(j.BaseURL, "/") + "/chat/completions"
	req, _ := http.NewRequest("POST", url, bytes.NewReader(chatBody(model, prompt)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+j.APIKey)
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("jev status %d: %s", resp.StatusCode, core.Clip(string(b), 200, 0))
	}
	return chatContent(b)
}

// ---------- latency stats ----------

type latStats struct {
	mu sync.Mutex
	m  map[string][]int64
	ok map[string][2]int
}

func (s *latStats) add(key string, ms int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m, s.ok = map[string][]int64{}, map[string][2]int{}
	}
	v := append(s.m[key], ms)
	if len(v) > 200 {
		v = v[len(v)-200:]
	}
	s.m[key] = v
	c := s.ok[key]
	if ok {
		c[0]++
	} else {
		c[1]++
	}
	s.ok[key] = c
}

type latSummary struct {
	Key  string `json:"key"`
	N    int    `json:"n"`
	P50  int64  `json:"p50_ms"`
	P90  int64  `json:"p90_ms"`
	Max  int64  `json:"max_ms"`
	OK   int    `json:"ok"`
	Fail int    `json:"fail"`
}

func summarize(key string, v []int64, ok, fail int) latSummary {
	s := append([]int64(nil), v...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	out := latSummary{Key: key, N: len(s), OK: ok, Fail: fail}
	if len(s) > 0 {
		out.P50, out.P90, out.Max = s[len(s)/2], s[(len(s)*9)/10], s[len(s)-1]
	}
	return out
}

func (s *latStats) summary() []latSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []latSummary{}
	for k, v := range s.m {
		out = append(out, summarize(k, v, s.ok[k][0], s.ok[k][1]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ---------- management ----------

type managementRequest struct {
	pluginapi.ManagementRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func jsonResp(code int, v any) ([]byte, error) {
	b, _ := json.Marshal(v)
	return okEnvelope(pluginapi.ManagementResponse{StatusCode: code, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: b})
}

// redacted hides the Jev key in API output.
func redacted(c core.Config) core.Config {
	if c.Classifier.Jev.APIKey != "" {
		c.Classifier.Jev.APIKey = "••••" + lastN(c.Classifier.Jev.APIKey, 4)
	}
	return c
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return ""
	}
	return s[len(s)-n:]
}

func handleManagement(raw []byte) ([]byte, error) {
	var req managementRequest
	_ = json.Unmarshal(raw, &req)
	p := req.Path
	switch {
	case strings.Contains(p, "/v0/resource/") && strings.HasSuffix(p, "/models.json"):
		return listModels()
	case strings.Contains(p, "/v0/resource/"):
		return okEnvelope(pluginapi.ManagementResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: panelHTML})
	case strings.HasSuffix(p, "/decisions"):
		return jsonResp(200, map[string]any{"version": pluginVersion, "decisions": router.Recent(200)})
	case strings.HasSuffix(p, "/state"):
		cfg := loadedConfig()
		_, saved := loadSaved(cfg.StateFile)
		return jsonResp(200, map[string]any{
			"version": pluginVersion, "config": redacted(cfg), "saved_override": saved, "state_file": cfg.StateFile,
			"available_providers": lastProviders.Load(), "sessions": router.Sessions(), "decisions": router.Recent(200), "active": router.Active(), "cooling": router.Cooling(),
			"classifier_latency": stats.summary(), "profiles": poolProfiles(cfg),
		})
	case strings.HasSuffix(p, "/config") && strings.EqualFold(req.Method, "GET"):
		return jsonResp(200, redacted(loadedConfig()))
	case strings.HasSuffix(p, "/config") && strings.EqualFold(req.Method, "DELETE"):
		cfg := loadedConfig()
		_ = os.Remove(cfg.StateFile)
		if y, ok := yamlConfig.Load().(core.Config); ok {
			currentConfig.Store(y)
		}
		return jsonResp(200, map[string]any{"ok": true})
	case strings.HasSuffix(p, "/config"):
		return saveConfig(req.Body)
	case strings.HasSuffix(p, "/models"):
		return listModels()
	case strings.HasSuffix(p, "/classifier-test"):
		return classifierTest(req.Body, req.HostCallbackID)
	}
	return jsonResp(404, map[string]string{"error": "not found"})
}

func saveConfig(body []byte) ([]byte, error) {
	old := loadedConfig()
	next := core.DefaultConfig()
	if err := json.Unmarshal(body, &next); err != nil {
		return jsonResp(400, map[string]string{"error": "invalid JSON: " + err.Error()})
	}
	next.StateFile = old.StateFile
	// keep the stored key when the panel sends back the masked one
	if strings.HasPrefix(next.Classifier.Jev.APIKey, "••••") || next.Classifier.Jev.APIKey == "" {
		next.Classifier.Jev.APIKey = old.Classifier.Jev.APIKey
	}
	next.Normalize()
	for i, p := range next.Pool {
		if p.Model == "" || p.Tier < 1 || p.Tier > 5 {
			return jsonResp(400, map[string]string{"error": fmt.Sprintf("pool row %d: model and tier 1-5 are required", i+1)})
		}
	}
	b, _ := json.MarshalIndent(next, "", "  ")
	saveMu.Lock()
	defer saveMu.Unlock()
	if dir := filepath.Dir(next.StateFile); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := next.StateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return jsonResp(500, map[string]string{"error": err.Error()})
	}
	if err := os.Rename(tmp, next.StateFile); err != nil {
		return jsonResp(500, map[string]string{"error": err.Error()})
	}
	currentConfig.Store(next)
	hostLog("info", "auto-router: config saved from panel")
	return jsonResp(200, map[string]any{"ok": true, "config": redacted(next)})
}

func classifierTest(body []byte, cb string) ([]byte, error) {
	var in struct {
		Kind   string `json:"kind"`
		Model  string `json:"model"`
		Effort string `json:"effort"`
		N      int    `json:"n"`
		Prompt string `json:"prompt"`
	}
	_ = json.Unmarshal(body, &in)
	cfg := loadedConfig()
	c := cfg.Classifier
	if in.Kind != "" {
		c.Kind = in.Kind
	}
	if in.Model != "" {
		c.Model, c.Effort = in.Model, in.Effort
	}
	if in.N <= 0 || in.N > 10 {
		in.N = 3
	}
	samples := []string{"hi, what is 2+2?", "Fix the off-by-one error in this loop: for i := 0; i <= len(a); i++ { sum += a[i] }", "Design a sharded, multi-region rate limiter with exactly-once semantics and explain the failure modes."}
	if in.Prompt != "" {
		samples = []string{in.Prompt}
	}
	type row struct {
		Prompt string `json:"prompt"`
		Ms     int64  `json:"ms"`
		Answer string `json:"answer"`
		Error  string `json:"error,omitempty"`
	}
	var rows []row
	var lat []int64
	fails := 0
	for i := 0; i < in.N; i++ {
		sp := samples[i%len(samples)] + fmt.Sprintf(" (#%d)", i+1)
		t := core.Turn{UserText: sp, NewTurn: true}
		st := time.Now()
		ans, err := classifyWith(c, core.BuildPrompt(t), time.Duration(cfg.Classifier.TimeoutMs)*time.Millisecond, cb)
		ms := time.Since(st).Milliseconds()
		r := row{Prompt: core.Clip(sp, 40, 0), Ms: ms, Answer: core.Clip(strings.TrimSpace(ans), 20, 0)}
		if err != nil {
			r.Error = core.Clip(err.Error(), 160, 0)
			fails++
		} else {
			lat = append(lat, ms)
		}
		stats.add("test:"+c.Kind+":"+c.Model+effSuffix(c.Effort), ms, err == nil)
		rows = append(rows, r)
	}
	return jsonResp(200, map[string]any{"model": c.Model + effSuffix(c.Effort), "kind": c.Kind, "rows": rows, "summary": summarize(c.Model, lat, len(lat), fails)})
}

func effSuffix(e string) string {
	if e == "" {
		return ""
	}
	return "(" + e + ")"
}

// availableModel is one (model, provider) pair the host can serve.
type availableModel struct {
	ID       string               `json:"id"`
	Provider string               `json:"provider"`
	Auths    int                  `json:"auths"`
	Thinking core.ThinkingProfile `json:"thinking"`
	Image    bool                 `json:"image"` // accepts image input (registry metadata or known families)
}

// listModels asks the host model registry (host.auth.models) for every model each
// credential serves, drops the router's own virtual models, and attaches the
// oh-my-pi thinking profile.
func listModels() ([]byte, error) {
	raw, err := callHost(pluginabi.MethodHostAuthModels, pluginapi.HostAuthModelsRequest{})
	if err != nil {
		return jsonResp(502, map[string]string{"error": "host.auth.models: " + err.Error()})
	}
	var resp pluginapi.HostAuthModelsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return jsonResp(502, map[string]string{"error": "decode host.auth.models: " + err.Error()})
	}
	cfg := loadedConfig()
	idx := map[string]int{}
	out := []availableModel{}
	for _, c := range resp.Credentials {
		prov := strings.ToLower(strings.TrimSpace(c.Provider))
		if prov == "" || prov == pluginName {
			continue
		}
		for _, m := range c.Models {
			m = strings.TrimSpace(m)
			if m == "" || cfg.IsVirtual(m) {
				continue
			}
			k := prov + "\x00" + m
			if i, ok := idx[k]; ok {
				out[i].Auths++
				continue
			}
			idx[k] = len(out)
			out = append(out, availableModel{ID: m, Provider: prov, Auths: 1, Thinking: core.Profile(m, prov)})
		}
	}
	// Plugin-served models (e.g. commandcode-go) are not in host.auth.models; pick them
	// up from this CPA's own /v1/models, keyed by owned_by.
	known, vision := map[string]bool{}, map[string]bool{}
	v1 := localV1Models()
	for _, om := range v1 {
		for _, x := range append(append([]string{}, om.InputModalities...), om.Arch.InputModalities...) {
			if strings.EqualFold(x, "image") {
				vision[om.ID] = true
			}
		}
	}
	for _, m := range out {
		known[m.ID] = true
	}
	for _, om := range v1 {
		if om.ID == "" || known[om.ID] || cfg.IsVirtual(om.ID) || om.OwnedBy == pluginName {
			continue
		}
		prov := strings.ToLower(om.OwnedBy)
		known[om.ID] = true
		out = append(out, availableModel{ID: om.ID, Provider: prov, Auths: 1, Thinking: core.Profile(om.ID, prov)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Provider < out[j].Provider
	})
	for i := range out {
		out[i].Image = core.KnownImage(out[i].ID) || vision[out[i].ID]
	}
	modelCache.Store(out)
	body := map[string]any{"models": out, "count": len(out)}
	if b, err := json.MarshalIndent(body, "", " "); err == nil {
		_ = os.WriteFile(filepath.Join(filepath.Dir(cfg.StateFile), "cpa-auto-router.models.json"), b, 0o644)
	}
	return jsonResp(200, body)
}

var startupOnce sync.Once

// logModelsAtStartup records the model picker contents once the host registry is populated.
func logModelsAtStartup() {
	startupOnce.Do(func() {
		go func() {
			time.Sleep(15 * time.Second)
			raw, _ := listModels()
			var env struct {
				Result struct {
					Body []byte `json:"body"`
				} `json:"result"`
			}
			_ = json.Unmarshal(raw, &env)
			var b struct {
				Count int `json:"count"`
			}
			_ = json.Unmarshal(env.Result.Body, &b)
			hostLog("info", fmt.Sprintf("auto-router: model picker has %d (model,provider) entries (host.auth.models + /v1/models)", b.Count))
		}()
	})
}

func poolProfiles(cfg core.Config) map[string]core.ThinkingProfile {
	m := map[string]core.ThinkingProfile{}
	for _, p := range cfg.Pool {
		m[p.Model+"@"+strings.ToLower(p.Provider)] = core.Profile(p.Model, p.Provider)
	}
	return m
}

type v1Model struct {
	ID      string `json:"id"`
	OwnedBy string `json:"owned_by"`
	// Optional modality metadata some registries expose.
	InputModalities []string `json:"supported_input_modalities"`
	Arch            struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
}

// imageFallback picks the best image-capable CPA model whose provider is routable.
func imageFallback(avail []string) (availableModel, bool) {
	ms, _ := modelCache.Load().([]availableModel)
	if len(ms) == 0 {
		listModels()
		ms, _ = modelCache.Load().([]availableModel)
	}
	best, bs := availableModel{}, -1.0
	for _, m := range ms {
		if !m.Image {
			continue
		}
		rp, ok := resolveProvider(m.Provider, avail)
		if !ok {
			continue
		}
		sc := core.PoolEntry{Model: m.ID}.ScoresFor()
		s := sc["general"] + sc["text"]
		if s > bs {
			m.Provider = rp
			best, bs = m, s
		}
	}
	return best, bs >= 0
}

// localV1Models reads port and the first client api key from ./config.yaml (never
// logged) and lists this CPA's /v1/models. Errors yield an empty list.
func localV1Models() []v1Model {
	b, err := os.ReadFile("config.yaml")
	if err != nil {
		return nil
	}
	port, key := 8317, ""
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n")
	pm := regexp.MustCompile(`^\s*port:\s*(\d+)`)
	for i, ln := range lines {
		if m := pm.FindStringSubmatch(ln); m != nil && port == 8317 {
			fmt.Sscanf(m[1], "%d", &port)
		}
		if key == "" && strings.TrimSpace(ln) == "api-keys:" && i+1 < len(lines) {
			if k := strings.TrimSpace(lines[i+1]); strings.HasPrefix(k, "- ") {
				key = strings.Trim(strings.TrimSpace(k[2:]), `"'`)
			}
		}
	}
	c := struct {
		Port    int
		APIKeys []string
	}{port, nil}
	if key != "" {
		c.APIKeys = []string{key}
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/v1/models", c.Port), nil)
	if len(c.APIKeys) > 0 {
		req.Header.Set("Authorization", "Bearer "+c.APIKeys[0])
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var r struct {
		Data []v1Model `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return nil
	}
	return r.Data
}
