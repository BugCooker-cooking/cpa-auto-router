package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/BugCooker-cooking/cpa-auto-router/core"
)

// Inline failover: model.route returns TargetKind=self for virtual-model requests, and the
// plugin's own executor runs the request through host.model.execute(_stream) with
// ForcedProvider, retrying the next candidate on retriable failures. Streams can only
// fail over before the first chunk has been forwarded to the client.

type inlineRoute struct {
	d     core.Decision
	cands []core.Candidate
	at    time.Time
}

var (
	inlineMu  sync.Mutex
	inlineMap = map[[32]byte]inlineRoute{}
)

func rememberInline(body []byte, d core.Decision, avail []string) {
	cands := []core.Candidate{{Model: d.Model, Provider: d.Provider, Target: d.TargetModel, Tier: d.ModelTier}}
	for _, c := range d.Fallbacks {
		if rp, ok := resolveProvider(c.Provider, avail); ok {
			c.Provider = rp
			cands = append(cands, c)
		}
	}
	inlineMu.Lock()
	defer inlineMu.Unlock()
	now := time.Now()
	for k, v := range inlineMap {
		if now.Sub(v.at) > 2*time.Minute {
			delete(inlineMap, k)
		}
	}
	inlineMap[sha256.Sum256(body)] = inlineRoute{d: d, cands: cands, at: now}
}

func takeInline(body []byte) (inlineRoute, bool) {
	inlineMu.Lock()
	defer inlineMu.Unlock()
	k := sha256.Sum256(body)
	r, ok := inlineMap[k]
	delete(inlineMap, k)
	return r, ok
}

type rpcExecRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func errStatus(err error) int {
	var he *hostCallError
	if errors.As(err, &he) {
		return he.Status
	}
	return 0
}

func failoverExecute(stream bool, raw []byte) ([]byte, bool) {
	var req rpcExecRequest
	if json.Unmarshal(raw, &req) != nil {
		return nil, false
	}
	ir, ok := takeInline(req.OriginalRequest)
	if !ok {
		return nil, false
	}
	cfg := loadedConfig()
	max := cfg.Routing.MaxAttempts()
	format := req.SourceFormat
	base := func(c core.Candidate, s bool) hostModelExecutionRequest {
		return hostModelExecutionRequest{HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: format, ExitProtocol: format, Model: c.Target, Stream: s, Body: req.OriginalRequest,
			Headers: req.Headers, Query: req.Query, Alt: req.Alt, ForcedProvider: c.Provider,
		}, HostCallbackID: req.HostCallbackID}
	}
	onFail := func(i int) func(core.Candidate, int, error) {
		return func(c core.Candidate, status int, err error) {
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			router.Cool(c.Model, cfg.Routing.Cooldown(), status, msg)
			var next *core.Candidate
			if j := indexOf(ir.cands, c) + 1; j < len(ir.cands) && j < max && core.Retriable(status, msg) {
				next = &ir.cands[j]
			}
			router.AddHop(ir.d.At, ir.d.Session, core.Hop{At: time.Now(), Model: c.Model, Status: status, Error: core.Clip(msg, 160, 0)}, next)
			to := "give up"
			if next != nil {
				to = "-> " + next.Provider + "/" + next.Target
			}
			hostLog("warn", fmt.Sprintf("auto-router: failover %s/%s failed (status %d %s); cooling %s; %s", c.Provider, c.Target, status, core.Clip(msg, 120, 0), cfg.Routing.Cooldown(), to))
		}
	}

	if !stream {
		var out pluginapi.HostModelExecutionResponse
		_, status, err := core.RunFailover(ir.cands, max, func(c core.Candidate) (int, error) {
			rawResp, err := callHost(pluginabi.MethodHostModelExecute, base(c, false))
			if err != nil {
				return errStatus(err), err
			}
			out = pluginapi.HostModelExecutionResponse{}
			if err := json.Unmarshal(rawResp, &out); err != nil {
				return 0, err
			}
			if out.StatusCode >= 400 {
				return out.StatusCode, fmt.Errorf("upstream status %d: %s", out.StatusCode, core.Clip(string(out.Body), 200, 0))
			}
			return out.StatusCode, nil
		}, onFail(0))
		if err != nil || status >= 400 {
			if status == 0 {
				status = 502
			}
			return errorEnvelopeStatus("upstream_error", errText(err), status), true
		}
		b, _ := okEnvelope(pluginapi.ExecutorResponse{Payload: out.Body, Headers: out.Headers})
		return b, true
	}

	// Streaming: open, read the first chunk; only then commit and pump the rest.
	var sid string
	var first []byte
	var headers map[string][]string
	_, status, err := core.RunFailover(ir.cands, max, func(c core.Candidate) (int, error) {
		rawResp, err := callHost(pluginabi.MethodHostModelExecuteStream, base(c, true))
		if err != nil {
			return errStatus(err), err
		}
		var sr pluginapi.HostModelStreamResponse
		if err := json.Unmarshal(rawResp, &sr); err != nil {
			return 0, err
		}
		if sr.StatusCode >= 400 {
			closeModelStream(sr.StreamID)
			return sr.StatusCode, fmt.Errorf("upstream status %d", sr.StatusCode)
		}
		rd, err := readModelStream(sr.StreamID)
		if err != nil || rd.Error != "" {
			closeModelStream(sr.StreamID)
			if err == nil {
				err = errors.New(rd.Error)
			}
			return errStatus(err), err
		}
		if rd.Done && len(rd.Payload) == 0 {
			closeModelStream(sr.StreamID)
			return 0, errors.New("empty upstream stream")
		}
		sid, first, headers = sr.StreamID, rd.Payload, sr.Headers
		return 200, nil
	}, onFail(0))
	if err != nil || status >= 400 || sid == "" {
		if status == 0 {
			status = 502
		}
		return errorEnvelopeStatus("upstream_error", errText(err), status), true
	}
	if req.StreamID == "" { // host expects inline chunks
		chunks := []pluginapi.ExecutorStreamChunk{{Payload: first}}
		for {
			rd, err := readModelStream(sid)
			if err != nil || rd.Error != "" {
				break
			}
			if len(rd.Payload) > 0 {
				chunks = append(chunks, pluginapi.ExecutorStreamChunk{Payload: rd.Payload})
			}
			if rd.Done {
				break
			}
		}
		closeModelStream(sid)
		b, _ := okEnvelope(map[string]any{"headers": headers, "chunks": chunks})
		return b, true
	}
	go pumpStream(req.StreamID, sid, first)
	b, _ := okEnvelope(map[string]any{"headers": headers})
	return b, true
}

func pumpStream(out, in string, first []byte) {
	_, _ = callHost(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": out, "payload": first})
	for {
		rd, err := readModelStream(in)
		if err != nil {
			_, _ = callHost(pluginabi.MethodHostStreamClose, map[string]any{"stream_id": out, "error": err.Error()})
			closeModelStream(in)
			return
		}
		if len(rd.Payload) > 0 {
			if _, err := callHost(pluginabi.MethodHostStreamEmit, map[string]any{"stream_id": out, "payload": rd.Payload}); err != nil {
				closeModelStream(in)
				return
			}
		}
		if rd.Error != "" || rd.Done {
			_, _ = callHost(pluginabi.MethodHostStreamClose, map[string]any{"stream_id": out, "error": rd.Error})
			closeModelStream(in)
			return
		}
	}
}

func readModelStream(id string) (pluginapi.HostModelStreamReadResponse, error) {
	var rd pluginapi.HostModelStreamReadResponse
	raw, err := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: id})
	if err != nil {
		return rd, err
	}
	err = json.Unmarshal(raw, &rd)
	return rd, err
}

func closeModelStream(id string) {
	if id != "" {
		_, _ = callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: id})
	}
}

func indexOf(cs []core.Candidate, c core.Candidate) int {
	for i, x := range cs {
		if strings.EqualFold(x.Model, c.Model) && x.Provider == c.Provider {
			return i
		}
	}
	return len(cs)
}

func errText(err error) string {
	if err == nil {
		return "auto-router: all failover attempts failed"
	}
	return "auto-router: all failover attempts failed; last error: " + err.Error()
}
