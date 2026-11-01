# CPA Auto Router 设计文档（v0.1 草案）

> 目标：为 CLIProxyAPI（CPA，`router-for-me/CLIProxyAPI`，Go，当前 v8）做一个开源的「自动模型路由」扩展。客户端请求模型 `auto`，扩展按「每个用户回合」分类一次、整回合保持，并路由到 CPA 已有的某个模型，同时决定思考强度；用户不满意时自动升档。
>
> 参考：magpie（`yetone/magpie`，MIT）的分类/路由思路；oh-my-pi（`can1357/oh-my-pi`，MIT）的「每模型内置思考档位 + 各厂商请求格式映射」。
> 调研日期：2026-10-31（北京时间）。代码均为当日 `main` 浅克隆。

---

## 0. 结论速览

- **CPA 已有正式插件机制**（`plugins:` 配置段 + C ABI 动态库 `.so/.dylib/.dll`，Go/C/Rust 示例齐全，还有插件商店 registry）。其中 `ModelRouter`、`ModelRegistrar`、`ManagementAPI`（含可浏览的 resource 页面）、`host.model.execute`（插件通过宿主调用任意模型）、`RequestInterceptor`、`RequestLifecyclePlugin`、`UsagePlugin` 恰好覆盖本需求。
- **推荐：做成 CPA 原生插件**（Go 写，`-buildmode=c-shared`），不 fork、不另起 sidecar。sidecar 作为备选（若某版本插件 ABI 不满足）。
- 思考强度：借鉴 oh-my-pi 的「**统一 effort 阶梯 + 每模型支持档位 + 每厂商 wire 模式**」三层模型；落到 CPA 时尽量用 CPA 自带的 `model(level)` 后缀/thinking 模块下发，插件只维护「每模型支持哪些档、默认档」的表。
- 能力分级：5 档 tier（T1 极速 → T5 旗舰推理），内置默认表 + 用户可覆盖 + 「校准向导」辅助敲定。
- 分类器：默认「用户在面板选的 CPA 模型」（测试默认 `gemini-3.8-flash` 经 CPA），可选 Jev（TypeSafe 决策 API）。
- 自动升档：下一回合用户表达不满 / review 步骤判不达标 → tier+1（封顶、冷却后逐步回落）。

---

## 1. CPA 现状调研

### 1.1 插件机制（官方）

源码位置：`sdk/pluginapi/types.go`（宿主侧能力 schema）、`sdk/pluginabi`（C ABI）、`internal/pluginhost/*`（加载/RPC/桥接，含 `loader_windows.go`、`loader_unix.go`）、`examples/plugin/*`。

`config.yaml`：

```yaml
plugins:
  enabled: true          # 默认关闭；动态库插件是“受信任的进程内代码”
  dir: "plugins"
  path: [ "/abs/path/cpa-auto-router.so" ]   # 或放入 dir / 走插件商店
  configs:
    cpa-auto-router:     # 键 = 插件名
      enabled: true
      priority: 50
      # ...插件自有配置（ConfigFields 会被管理面板读出来渲染）
```

与本项目相关的能力（`pluginapi.Capabilities`）：

| 能力 | 用途 |
|---|---|
| `ModelRegistrar` / `ModelProvider` | 把虚拟模型 `auto`（以及 `auto-fast`/`auto-max` 等预设）注册进 CPA 模型表 → 自动出现在 `/v1/models` |
| `ModelRouter.RouteModel(ModelRouteRequest)` | **核心**。在「模型→provider 解析、凭据选择」之前被调用；拿到 `RequestedModel`、`SourceFormat`、`Headers`、原始 `Body`、`AvailableProviders`；返回 `Handled=true, TargetKind=provider, Target=<provider>, TargetModel=<真实模型>, Reason=...` 即可把请求交给 CPA 内置链路执行（协议翻译、凭据轮询、重试全复用） |
| `host.model.execute` / `execute_stream` | 插件内部调用任意 CPA 模型 —— 用来跑分类器、review，且凭据/代理都走 CPA；带 `host_callback_id` 时不会递归触发本插件 |
| `RequestInterceptor`（before/after auth，v8 起支持 body 字节区间 patch） | 兜底：需要改 body（剥离不兼容采样参数、写入 effort 字段）时使用 |
| `RequestLifecyclePlugin` / `UsagePlugin` | 拿到每个请求的终态与 usage → 统计延迟、token、失败率，供状态面板 |
| `ManagementAPI` | 在 `/v0/management/...` 下注册插件自有 API，并在 `/v0/resource/plugins/<id>/` 暴露**可浏览的页面**（我们的配置/状态 UI 就挂这里） |
| `ConfigFields` | 声明配置字段，官方管理面板可直接渲染简单表单 |

官方示例 `examples/plugin/claude-web-search-router` 就是「ModelRouter + host.model.* 回调」的同类用法，可直接作为脚手架。

### 1.2 管理 API 与管理面板

- 管理 API：`/v8/management`（兼容 `/v0`），`Authorization: Bearer <management-key>` 或 `X-Management-Key`。
  - `GET/PUT/PATCH /config`、`/config.yaml`、`/config/<section>/<field>`；插件配置路径：`/config/plugins/configs/<id>`（**我们的面板保存配置就走这里**，CPA 会热加载）。
  - `GET /credentials`、`/credentials/models`（各凭据/auth 文件可用模型）、`/routing/model-definitions/<channel>`、`/observability/usage/*`、`/observability/logs/*`、`/plugins`、`/plugins/store`…
- 管理面板：独立仓库 `router-for-me/Cli-Proxy-API-Management-Center`（`config.yaml` 中 `panel-github-repository`），CPA 下载静态资源后托管。插件列表会读取插件的 `Logo`、`ConfigFields` 展示。
- `/v1/models`：来自 CPA 模型注册表（各凭据 auth 文件 + 配置的 api-keys/兼容 provider + 插件 `ModelRegistrar`）。
- CPA 已有思考配置模块 `internal/thinking`：支持模型名后缀 `gpt-5.2(high)`、`claude-sonnet-4-5(16384)`、`(none)`、`(auto)`，并按 provider（claude/codex/gemini/openai/kimi/xai/antigravity…）转换成各自 wire 字段。**这意味着我们大多数情况下只要返回 `TargetModel="xxx(high)"` 即可**（需在 M0 验证 RouteModel 的 TargetModel 是否走后缀解析，见 §9 Q1）。

### 1.3 架构选型

| 方案 | 优点 | 缺点 |
|---|---|---|
| **A. CPA 原生插件（推荐）** | 官方扩展点、无需 fork；复用 CPA 协议翻译/凭据/重试/思考映射；`auto` 原生出现在 `/v1/models`；管理 API+资源页可挂 UI；可上插件商店 | 动态库进程内运行，崩溃影响 CPA；需跟随插件 schema 版本；Windows 需 cgo 编译 `.dll`（mingw） |
| B. Sidecar 反代（放 CPA 前） | 与 CPA 解耦、语言自由、崩溃隔离 | 要自己解析三种协议（OpenAI chat / Responses / Anthropic / Gemini）改 model 与 effort；双跳延迟；`/v1/models` 需合并；鉴权要转发；面板分离 |
| C. Fork/patch CPA | 能力最全 | 维护成本极高，CPA 发版频繁；用户必须换二进制 |

**推荐 A**：路由决策用 `ModelRouter`（只读 body，返回目标），分类/review 用 `host.model.execute`，指标用 Lifecycle/Usage，UI 用 ManagementAPI resource。代码按「core 库 + 适配层」组织，core（分类、tier、升档、会话状态）与 CPA 无关，保留未来做 sidecar 版本（B）的可能。

---

## 2. 思考强度（effort）——借鉴 oh-my-pi，magpie 为辅

### 2.1 oh-my-pi 的做法（`packages/catalog`）

三层：

1. **统一 effort 阶梯**：`minimal < low < medium < high < xhigh < max`（另有 `off`）。
2. **每模型 ThinkingConfig**（`catalog/src/types.ts`），构建期从规则烘焙：
   - `mode`：wire 模式，五选一 `effort`（OpenAI `reasoning_effort` / Responses `reasoning.effort`）、`budget`（token 预算：Anthropic 旧 `thinking.budget_tokens`、Gemini 2.5 `thinkingBudget`）、`google-level`（Gemini 3 `thinkingLevel`）、`anthropic-adaptive`（新 Claude：`thinking:{type:adaptive}` + `output_config.effort`）、`anthropic-budget-effort`（预算+effort 并存）。
   - `efforts`：该模型支持的档（有序），`defaultLevel`：默认档。
   - `effortMap`：统一档→厂商 wire 值重映射（如 `minimal` 在 Anthropic adaptive 不存在→夹到 `low`）。
   - `effortBudgets`：每档 token 预算（如 Gemini 2.5 Pro：minimal 128 / low 2048 / medium 8192 / high 32768；Flash high 24576）。
   - `effortRouting`：有的厂商把档位做成不同模型 ID（如 `-low` SKU），按档换 wire 模型名。
   - `requiresEffort`（`thinking-requires-effort`）：不能关思考（o 系列、Gemini 2.5 Pro…），请求 off 时夹到最低/默认档。
   - `supports-sampling-params #false`：o 系列与 GPT-5+ 带 `temperature/top_p` 会 400 → 必须剥离。
3. **规则文件（KDL）按「厂商谱系 class → 家族 family → 版本 revision → 托管方 on "<provider>"」级联**（`compat/rules/classes/*.kdl`）。示例：
   - gemini：Pro <3 → minimal/low/medium/high；Pro ≥3 → 仅 low/high；Flash/Lite → minimal/low/medium/high。
   - openai：GPT <5.2 → minimal..high；5.2+ 去掉 minimal 加 xhigh；5.6+ → low..max 五档。
   - deepseek：V4 pro/flash → low/high/max（medium/xhigh 折叠到 high）；r1/v3/reasoner → high/max；OpenRouter 上 V4 仅 high。
   - gpt-oss：low/medium/high（个别托管方不同）。
   - 夹取规则 `clampThinkingLevelForModel`：不支持的档先查 effortMap，否则**向下**取最近档。

### 2.2 magpie 的补充经验（`internal/gateway`、`internal/provider/models.go`）

- `fitEffort`：取模型自有档中最接近的，**平局向上**；`ultra`→`max`。
- Anthropic 档→预算：low 4096 / medium 10000 / high 24000 / xhigh,max 32000（反向映射也有）。
- 运行期自学习：上游回 `thinking.type must be enabled` → 记住该模型「必须开思考」；Claude 拒绝 `temperature/top_p/top_k` → 记住并剥离（`samplingRefused`）；Kimi「temperature 只能为 1」；Claude 思考签名跨账号/会话失效 → 丢弃对应 thinking block 重试（**换模型路由时尤其重要**）。
- 用户可把模型标为 `Reasonless`（无思考）。

### 2.3 本项目方案

- 插件内置一份 **effort 能力表**（YAML，嵌入二进制，可在线更新），结构仿 oh-my-pi，但只保留路由需要的字段：

```yaml
# effort-profiles.yaml（内置，用户可覆盖）
- match: { class: gemini, family: flash, revision: ">=3" }
  mode: google-level
  efforts: [minimal, low, medium, high]
  default: low
- match: { class: gemini, family: pro, revision: ">=3" }
  mode: google-level
  efforts: [low, high]
  default: high
  requires_effort: true
- match: { class: openai, family: gpt, revision: ">=5.6" }
  mode: effort
  efforts: [low, medium, high, xhigh, max]
  default: medium
  sampling_params: false
- match: { class: anthropic, family: opus, revision: ">=4.7" }
  mode: anthropic-adaptive
  efforts: [low, medium, high, xhigh, max]
  default: high
- match: { class: deepseek, family: [pro, flash, v4] }
  mode: effort
  efforts: [low, high, max]
  default: high
```

- 下发路径（优先级）：
  1. 返回 `TargetModel = "<model>(<level>)"`，由 CPA `internal/thinking` 转换为 wire 字段（最省事，CPA 已覆盖 claude/codex/gemini/openai/kimi/xai/antigravity）。
  2. CPA 不认识的模型/模式 → `RequestInterceptor` 用 byte-range patch 写入字段并剥离被拒的采样参数。
- **effort 决定**：`最终档 = clamp( 用户覆盖 ?? 分类器建议 ?? 模型默认档 , 模型支持档 )`；客户端自己显式带了 effort（如 Codex 的 `reasoning.effort`）时，默认**尊重客户端**（可配置为“由 auto 接管”）。
- 生成脚本：可从 oh-my-pi 的 `rules.json`（MIT，保留版权声明）导出初版表，后续人工维护。

---

## 3. 模型池与能力分级

### 3.1 Tier 方案（5 档）

| Tier | 名称 | 典型任务 | 默认 effort 倾向 | 示例（仅默认建议） |
|---|---|---|---|---|
| T1 | 极速 | 闲聊、翻译、格式化、简单问答 | off/minimal | Gemini Flash-Lite 类、小模型 |
| T2 | 轻量 | 单文件小改、解释代码、一般写作 | low | Gemini 3.x Flash、GPT mini、DeepSeek flash |
| T3 | 标准 | 常规编码、多步工具调用 | medium | Claude Sonnet、GPT 主力、Gemini Pro(low) |
| T4 | 强 | 复杂调试、架构设计、跨文件重构 | high | Claude Opus、GPT 旗舰 high、Gemini Pro(high) |
| T5 | 旗舰推理 | 极难推理、长链规划、前一档失败兜底 | xhigh/max | 各家最强 + max |

同一底模不同 effort 可算不同条目（如 `gpt-x(medium)` 放 T3、`gpt-x(xhigh)` 放 T5），让 tier 同时表达「模型+强度」。

### 3.2 如何敲定分级（建议三步）

1. **内置默认**：维护一份 `model-tiers.yaml`（按 class/family/revision 匹配，同 effort 表），新模型落入匹配规则；匹配不到 → 标记「未分级」，不进池，面板提示。
2. **参考数据辅助**：面板展示公开基准（如 LMArena / Aider / SWE-bench 分数，手动或定期拉取）+ 价格 + 实测平均延迟，按综合分给出「建议 tier」，用户一键采纳或拖拽调整。
3. **本地校准（可选）**：提供 10–20 道分档测试题（每档若干），对池中模型批量跑一次，由强模型/人工评分 → 输出建议 tier；运行中再用「升档率」反馈：某模型被升档率持续偏高 → 面板提示“建议下调一档”。

用户最终拥有决定权：每个池成员可设 `tier`、`weight`、`enabled`、`effort_default`、`effort_allowed`、`cost_hint`。

### 3.3 同档内选择

同 tier 多个模型：按 `weight` 加权 + 健康度（最近失败率、冷却中则跳过）+ **会话粘性**（同一会话已在该模型上则继续，保护 prompt cache 与 Claude thinking 签名）。

---

## 4. 分类器

### 4.1 输入构造（照搬 magpie）

- 剥离 agent 注入内容：`<system-reminder>…`、`<environment_context>…`、IDE 附带的上下文块等（规则表可配置）。
- 当前用户消息：头 3000 rune + 尾 1000 rune；附上**上一条用户消息**与**最后一条 assistant 回复**（各截 1000）。
- 只在「新用户回合」分类：判断依据为最后一条消息是真实用户文本（非 tool_result / function_call_output）；工具循环中的后续请求复用本回合决策。

### 4.2 Prompt 与输出

- 输出一个编号（数字），对应 1..5 tier；同时输出 effort 档与 `dissatisfied` 标志。推荐一次请求返回紧凑 JSON 或固定格式 `tier=3;effort=medium;unhappy=0`，解析失败则只取首个数字。
- 「继续/continue/接着做/go on」等短指令：直接沿用上一回合决策（不调用分类器）。

### 4.3 可靠性参数（来自 magpie）

- 超时 8s；失败后 30s 内不再调用（rest）；同一（消息摘要+配置版本）结果缓存 10 分钟；失败回退：上一回合的决策 → 会话无历史则 `default_tier`（默认 T3）。

### 4.4 分类器选择

- `classifier.kind: model`（默认）：任意 CPA 模型，经 `host.model.execute` 调用；**测试默认 `gemini-3.8-flash`**，effort=minimal/off，max_tokens 很小。
- `classifier.kind: jev`：TypeSafe Jev，`POST /v1/systemone`，`state`=构造好的文本，`questions` 用 `score`（tier 1–5）+ `choice`（effort）+ `noul`（是否不满意）。官方价约 $0.042/M 输入 token，输出免费，延迟低。需要用户自备 API key（存环境变量，不入配置文件）。

---

## 5. 路由流程

```
client --(model=auto)--> CPA handler
  └─ ModelRouter.RouteModel(req)
       1. RequestedModel ∉ {auto, auto-*} → Handled=false
       2. session_key = hash(客户端 API key 作用域 + 会话标识)
          会话标识优先级：header(x-session-id / anthropic metadata.user_id / prompt_cache_key / Codex conversation id) → 首条用户消息哈希
       3. 判断是否新用户回合；否 → 用回合缓存决策
       4. 是 → 并行：分类(tier, effort, unhappy) ；
          升档逻辑（§6）调整 tier
       5. 在 tier 内选模型（粘性/权重/健康），算 effort（§2.3）
       6. 返回 {Handled, TargetKind: provider, Target: <provider>, TargetModel: "model(level)", Reason}
       7. 记录 decision（供面板）；Lifecycle 回调补上延迟/状态/usage
  └─ CPA 原链路执行（翻译、凭据、重试）
失败（上游 4xx/5xx 不可重试）→ 可选“同档换模型/降档”回退（M3）
```

注意跨模型切换的兼容处理：切到不同厂商时，历史中的 Claude thinking 签名块、加密 reasoning 等可能被拒 → 依赖 CPA 翻译器；如遇拒绝，参照 magpie `thinking_bound.go` 在 interceptor 中剥离后重试（M3）。

---

### 5.1 粘性路由（v0.3.6，可选）

`routing.sticky: true` 时，加权排序后若会话当前模型仍在候选列表中（已按虚拟模型档位范围、图片能力过滤）且 `top.final - cur.final ≤ routing.sticky_margin`（默认 0.8），沿用当前模型并在决策上记 `sticky: true`、`sticky_top: <原最高分模型>`、信号 `sticky`。若当前模型档位低于所需档位且最高分模型档位更高（升级场景）则不粘。目的：减少模型切换以提高提示词缓存命中率。

## 6. 自动升档（Auto-Escalation）

### 6.1 触发信号

1. **用户不满**：新回合分类时同时判断（分类 prompt 中加入上一条 assistant 回复摘要），信号如“不对/还是不行/没解决/重新来/you're wrong”。同时有轻量关键词预筛作为兜底（中英文词表）。
2. **Review 不达标**：
   - 被动：客户端（如 Claude Code 子 agent / 自定义 review 步骤）的输出中出现约定标记，如 `REVIEW: FAIL` / `不符合预期`，在 Lifecycle 或 Response 拦截中检测；
   - 主动（可选，费钱）：对每回合最终 assistant 输出，异步调用 review 模型打分（0–1），低于阈值则标记该会话“下回合升档”。
3. （可选）硬失败：上游连续报错、输出被截断/空回复。

### 6.2 规则

- `escalation_level` 存于会话状态；本回合 tier = `clamp(base_tier + escalation_level, 1, max_tier)`。
- 触发一次 +1（同一回合最多 +1；可配置 `step`）；上限 `max_tier`（默认 5）和 `max_escalation`（默认 +2）。
- 若已在该 tier 顶层，先尝试同 tier 内的 effort 提档（如 high→xhigh），再升 tier。
- **回落（decay）**：连续 N 个（默认 3）无负面信号的回合 −1；或会话空闲超过 30 分钟清零。新话题（分类器判 tier 明显降低）也可直接清零（可配置）。
- 每次升/降都写入决策日志（原因、触发证据片段 ≤200 字）。

---

## 7. 配置与数据结构

### 7.1 插件配置（`plugins.configs.cpa-auto-router`）

```yaml
enabled: true
priority: 50
virtual_models:
  - name: auto              # 全池
  - name: auto-fast         # 限制 max_tier: 2
    max_tier: 2
  - name: auto-max
    min_tier: 4
default_tier: 3
respect_client_effort: true
classifier:
  kind: model               # model | jev
  model: gemini-3.8-flash
  effort: minimal
  timeout_ms: 8000
  rest_after_failure_s: 30
  cache_ttl_s: 600
  jev: { model: jev-1.14.0, api_key_env: TYPESAFE_API_KEY, base_url: "https://api.typesafe.ai" }
input:
  head_runes: 3000
  tail_runes: 1000
  prev_cap_runes: 1000
  strip_tags: [system-reminder, environment_context]
continue_words: [continue, 继续, 接着, go on]
pool:
  - model: gemini-3.8-flash
    tier: 2
    weight: 1
    effort_default: low
    effort_allowed: [minimal, low, medium, high]
  - model: claude-sonnet-x
    tier: 3
  - model: gpt-x
    tier: 4
    effort_default: high
escalation:
  enabled: true
  on_user_unhappy: true
  on_review_fail: true
  review_markers: ["REVIEW: FAIL", "不符合预期"]
  active_review: { enabled: false, model: gemini-3.8-flash, threshold: 0.5 }
  step: 1
  max_escalation: 2
  max_tier: 5
  decay_after_turns: 3
  reset_idle_minutes: 30
sticky_sessions: { enabled: true, ttl_minutes: 60 }
effort_overrides:           # 覆盖内置表
  "deepseek-v4-pro": { efforts: [low, high, max], default: high }
storage: { decisions_keep: 5000, path: "plugins/cpa-auto-router/state.db" }
```

### 7.2 运行时数据

```go
type Session struct {
    Key             string
    Model           string    // 粘性模型
    BaseTier        int
    EscalationLevel int
    CleanTurns      int
    LastTurnHash    string
    LastDecision    Decision
    UpdatedAt       time.Time
}
type Decision struct {
    ID, SessionKey, VirtualModel string
    Turn            int
    Tier, BaseTier  int
    Model, Provider, Effort string
    Reason          string   // "classifier:3", "continue", "escalated:unhappy", "fallback:prev"...
    Classifier      string
    ClassifyMs      int
    UpstreamMs      int      // lifecycle 回填
    Status          int
    InTok, OutTok   int
    At              time.Time
}
```

存储：内存 LRU + 可选 SQLite（纯 Go `modernc.org/sqlite`，避免额外 cgo 依赖）环形保留。

---

## 8. API 与 UI

### 8.1 插件 Management API（`/v0/management/plugins/cpa-auto-router/...`，沿用 CPA 管理密钥）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/status` | 版本、分类器健康、最近错误、池概况 |
| GET | `/models` | CPA 可用模型（读宿主模型表/`credentials/models`）+ 内置 tier/effort 建议 |
| GET/PUT | `/config` | 读写插件配置（内部转 `PATCH /v8/management/config/plugins/configs/<id>`） |
| GET | `/decisions?session=&limit=` | 决策日志 |
| GET | `/sessions` / DELETE `/sessions/<key>` | 会话状态、手动清除升档 |
| POST | `/classify/test` | 输入一段文本，返回分类结果与耗时（调试） |
| POST | `/calibrate` | 运行校准题集（异步，返回任务 id） |
| GET | `/metrics` | 每模型请求数、平均/p95 延迟、失败率、升档率 |

### 8.2 UI（`/v0/resource/plugins/cpa-auto-router/`，单页静态资源嵌入插件）

```
┌ Auto Router ─────────────────────────────── [配置] [状态] [调试] ┐
│ 配置                                                             │
│  分类器: (●) CPA 模型 [gemini-3.8-flash ▾]  ( ) Jev  超时[8s]     │
│  模型池                                         [+ 从CPA添加]     │
│  ┌ T5 ┐ gpt-x(xhigh)  默认effort[xhigh▾] 权重[1] ☑               │
│  ├ T4 ┤ claude-opus   默认effort[high▾]  权重[1] ☑               │
│  ├ T3 ┤ claude-sonnet ...      (可拖拽改档；“建议: T3” 徽标)       │
│  ├ T2 ┤ gemini-3.8-flash                                          │
│  └ T1 ┘ (空)                                                      │
│  升档: ☑用户不满 ☑review失败 步长[1] 上限[+2] 回落[3回合]           │
├──────────────────────────────────────────────────────────────────┤
│ 状态                                                             │
│  会话   | 当前模型        | Tier(基+升) | effort | 最近原因   | 分类ms | 上游ms │
│  a1f3.. | claude-opus     | 3+1         | high   | 用户不满   | 420    | 9.8s   │
│  点开会话 → 每回合决策时间线（tier、模型、原因、证据片段、tokens）   │
│  模型统计：请求数 / p50 / p95 / 失败率 / 升档率                       │
├──────────────────────────────────────────────────────────────────┤
│ 调试：粘贴文本 → 分类结果；校准任务                                  │
└──────────────────────────────────────────────────────────────────┘
```

另提供响应头（可配置开启）：`X-Auto-Router-Model`、`X-Auto-Router-Tier`、`X-Auto-Router-Reason`，方便客户端侧排查。

---

## 9. 开放问题 / 风险

1. **TargetModel 是否解析 `(level)` 后缀**：需在 M0 写个最小插件验证 `ModelRouteResponse.TargetModel="gemini-3.8-flash(low)"` 能否触发 `internal/thinking`；若不行改用 RequestInterceptor 写 body。
2. **ModelRouter 是否对每个 tool-loop 请求都调用**：预期是；需确认 Responses API WebSocket / Codex 远程 compact 等路径是否也经过 ModelRouter。
3. **会话标识**：不同客户端（Claude Code、Codex、Cherry Studio、Cline…）的会话 id 来源不一，需逐个确认；无 id 时只能用首条消息哈希。
4. **Windows 构建**：Go `c-shared` 需 mingw-w64；CPA 加载 DLL 会复制到临时路径（`loader_windows.go`），需验证热更新与杀软误报。
5. **插件 schema 版本**：CPA 迭代快（当前 v8，schema v7/v8 特性），要声明最低版本并跟踪。
6. **跨厂商切换的历史兼容**：thinking 签名、加密 reasoning、tool call id 格式；粘性策略能缓解，升档换厂商时仍可能触发，需要实测。
7. **分级的主观性**：默认 tier 表谁来维护、多久更新；是否接入公开榜单自动建议。
8. **主动 review 成本**：每回合多一次模型调用，默认关闭。
9. **Jev 许可/条款**：Jev 是 TypeSafe 的闭源托管 API（按输入 token 计费 ~$0.042/M，输出免费；称不训练用户数据，ZDR 仅企业版）。未找到允许/限制「在开源项目中集成或转售」的明确条款；本项目仅做「用户自带 key 的可选客户端」，不内置 key、不默认启用，应在 README 标注第三方服务与隐私（对话片段会发给 TypeSafe），正式发布前需读其 ToS 确认。magpie、oh-my-pi 均为 MIT，借鉴代码/规则需保留版权声明。

---

## 10. 里程碑与工作量估计（1 人）

| 里程碑 | 内容 | 估时 |
|---|---|---|
| M0 验证 | 最小 Go 插件：注册 `auto`、RouteModel 固定路由到某模型、验证后缀 effort、Windows DLL 加载、host.model.execute 调分类器 | 2–3 天 |
| M1 MVP | 输入构造/剥离、分类（CPA 模型）、回合缓存与 continue、tier 池+粘性、effort 表（主流 6 家）、基础决策日志 API | 5–7 天 |
| M2 升档 | 不满检测、review 标记、升/降档状态机、Lifecycle 指标 | 3–4 天 |
| M3 稳健 | 失败回退/换模型、采样参数剥离、thinking 签名处理、Jev 分类器 | 4–5 天 |
| M4 UI | 配置页（池拖拽、effort）、状态页、调试页 | 4–6 天 |
| M5 分级辅助 | 内置 tier 表、校准题集、升档率建议；文档、发布到插件 registry | 3–5 天 |
| 合计 | | **约 21–30 人日（4–6 周）** |

测试策略：用 Gemini 3.8 Flash（经 CPA）作分类器跑 e2e；单测覆盖输入截断、continue、升/降档状态机、effort 夹取；录制若干 Claude Code / Codex 真实请求作回放夹具。

## 自动故障转移（0.3.6）

- **触发**：`core.Retriable(status, err)` —— 0（连接错误）、408、429、5xx、401/403/404，或错误文本含 `auth_not_found` / `timeout` / `connection`。其余 4xx 视为请求问题，不转移。
- **冷却**：`Router.cool` 按基础模型名记录 `until`；`Decide` 先得出完整候选顺序（`order()`：加权模式按最终得分，档位模式按与目标档位的距离，均已过滤图片能力与档位范围），剔除冷却中的模型（记入 `Decision.Cooled`，信号 `skip-cooling`）；全部冷却时保留原顺序并置 `AllCooling`。剩余候选写入 `Decision.Fallbacks`。过期即视为恢复，成功的用量记录也会立即解除冷却。
- **默认（跨请求）路径**：`usage.handle` 收到失败记录 → `Router.FailedUsage` 冷却该模型并返回 true，跳过失败升档。下一个请求自然绕开它。
- **同请求路径（实验，`failover_inline`）**：`model.route` 返回 `TargetKind=self`，决策按请求体 SHA-256 暂存（2 分钟）。插件执行器取回候选列表（主选 + Fallbacks，提供方经 `resolveProvider` 解析），用 `core.RunFailover` 依次调用 `host.model.execute` / `host.model.execute_stream`（`ForcedProvider`、`Model=目标(强度)`、入口/出口协议均为原始格式、原始请求体与请求头、`host_callback_id`）。每次失败调用 `Router.AddHop` 记录跳转并把决策切到下一个模型。流式在读到首个数据块前失败可切换，之后通过 `host.stream.emit/close` 转发。
- **上限**：`failover_max_attempts`（默认 3，最多 5）；不可重试错误立即返回。
