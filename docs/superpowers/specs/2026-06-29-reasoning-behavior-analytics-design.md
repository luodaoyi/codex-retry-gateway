# codex-retry-gateway reasoning 行为统计大盘设计

日期：2026-06-29

状态：主线程方向确认，先落盘方案，后续分阶段实现

关联资料：

- `docs/superpowers/specs/2026-06-29-reasoning-intercept-mode-design.md`
- `docs/superpowers/specs/2026-06-28-model-family-consistency-design.md`
- `docs/superpowers/specs/2026-06-28-model-contract-falsification-design.md`
- GitHub Issue: `https://github.com/nonononull/codex-retry-gateway/issues/9`

---

## 1. 背景

当前网关已经支持：

- 观测 `reasoning_tokens`
- 按配置命中 `reasoning_equals`
- 对命中响应做网关内重试
- 统计规则命中总数、实际拦截总数、实际拦截占比
- 展示模型声明一致性与主动探针结果

但现有设计仍然偏向“固定值规则”，例如 `reasoning_tokens = 516`。

用户现在明确要把方向升级为：

- 不再把 `516` 直接等同于“降智”
- 先做全量详细采集，再做数据统计与特征观察
- 通过大盘、图表、历史记录慢慢发现可疑特征
- 后续用“特征组合”匹配，而不是只用单一 token 值匹配
- 当前拦截配置可以继续自定义，本次不碰现有功能面与拦截面

一句话总结：

- `516` 仍是当前最重要的已验证样本值之一，但不是最终判定语义。
- 当前 516 拦截是粗颗粒全拦策略，存在误伤，因此更需要采集与特征定位来区分“普通观察 516”和“候选复盘 516”。

### 1.1 本轮会话统一口径

这几轮会话已经把设计口径统一成下面几条：

1. 现阶段不改现有拦截配置，当前功能面保持原样；拦截面可以继续按既有规则自定义，本次不新增、不替换、不收缩。
2. 采集面必须全量详细落盘，包含所有请求尝试和所有重试请求；记录请求使用的模型、模型家族、`reasoning.effort`、请求/响应时间、token、结构信号、重试信号与状态信息。
3. 真正要提炼的特征案例，现阶段先按你确认的基线走：`reasoning_tokens` 异常值 + `final_answer only` + `commentary_not_observed` + 时序归一化偏差。
4. `耗时 / TPS / token 长度` 只能作为一个复合归一化维度一起看，不能拆成三个独立特征。
5. 后续策略会从“当前 516 全拦规则”逐步演进到“按特征组合拦截”，但前提仍是先把证据和分布做完整。

---

## 2. 核心原则

第一阶段只做统计，不做新的自动拦截判断。

原则：

1. 先记录证据，再谈识别。
2. 先看分布，再谈特征。
3. 先做观察模式，再升级拦截模式。
4. 不把任何单一数值写成“已确认降智”。
5. 所有“可疑”结论都必须能回看样本证据。
6. 采集必须尽可能详细，所有请求尝试和重试请求都要纳入，但保存边界仍然只限结构化元数据。
7. 当前拦截配置与功能面不因本次方案改变，当前 516 全拦与其他既有自定义规则都先原样保留。
8. Codex 上下文压缩 / 维护请求必须与普通回答请求区分；`remote_compaction_v2` 这类请求只观察和落盘，不参与当前拦截规则，避免 `reasoning_tokens=0/null` 导致压缩失败。

UI 文案必须避免：

- `516 = 降智`
- `final_answer only = 降智`
- `耗时短 = 降智`

推荐文案：

- `候选异常特征`
- `疑似短路响应`
- `疑似省略推理过程`
- `reasoning_tokens=516` 聚集
- `时序归一化偏差`
- `统计相关性，不代表最终归因`

---

## 3. 为什么不能直接用耗时

“耗时短”不能直接作为异常特征。

总耗时至少受下面因素影响：

- 输入 token 数
- 输出 token 数
- reasoning token 数
- 上游 TPS
- 首 token 延迟
- 是否流式
- 是否网关内重试
- 网络波动
- 上游排队时间

因此第一阶段不能做：

- `duration_ms < X` 就判可疑

必须先把时序拆开，再和 token 规模、TPS 建立对应关系。

---

## 4. 第一阶段目标

第一阶段只建设“统计与证据底座”。

目标：

1. 为每一次请求尝试记录尽可能完整的特征样本。
2. 记录请求使用的模型、模型家族、`reasoning.effort`、请求时序、响应时序、token 数、结构信号与重试信号。
3. 建立 `duration / tokens / TPS / reasoning_tokens / 响应结构` 的关系。
4. 在 UI 中展示 reasoning 行为大盘。
5. 支持随时查看最近样本与聚合统计。
6. 保持现有拦截配置和功能面不变，当前 516 全拦与其他既有自定义规则都原样保留。
7. 不新增新的特征拦截规则。

---

## 5. 非目标

第一阶段不做：

1. 不自动判定“降智”。
2. 不替换现有 `reasoning_equals` 拦截规则。
3. 不训练分类器。
4. 不做 embedding / 风格识别。
5. 不直接根据耗时做拦截。
6. 不把主动探针样本混入真实代理请求统计。
7. 不无限持久化全量请求正文或响应正文。

---

## 6. 样本数据模型

新增请求尝试行为样本，建议命名：

```js
reasoning_behavior_samples
```

单条样本建议字段：

```js
{
  sample_id,
  ts,
  request_id,
  attempt_id,
  path,
  method,
  is_streaming,
  request_kind,
  intercept_exempt_reason,
  model,
  model_family,
  reasoning_effort,
  upstream_model,
  system_fingerprint,

  request_started_at,
  upstream_fetch_started_at,
  upstream_headers_at,
  first_stream_chunk_at,
  first_content_at,
  final_chunk_at,
  request_finished_at,

  duration_total_ms,
  upstream_wait_ms,
  time_to_first_chunk_ms,
  time_to_first_content_ms,
  stream_duration_ms,

  input_tokens,
  reasoning_tokens,
  output_tokens,
  total_tokens,

  output_tps,
  visible_output_tps,
  total_observed_tps,
  reasoning_adjusted_tps,

  has_commentary,
  has_final_answer,
  final_answer_only,
  has_tool_call,
  event_type_counts,
  response_item_type_counts,

  matched_current_rule,
  blocked_by_gateway,
  internal_retry_attempt_index,
  internal_retry_remaining,
  final_action,

  http_status,
  error_code,
  error_type,
  evidence_log_seq_range
}
```

说明：

- `sample_id` 用于 UI 回看。
- `request_id` 如果能从请求或响应上下文提取，则记录；没有则用本地生成 id。
- `attempt_id` 用于标记同一次请求下的具体尝试，内部重试必须单独编号。
- `request_kind` 用于区分普通回答请求和 `context_compaction` 等维护请求。
- `intercept_exempt_reason` 用于记录样本为何只观察不拦截，例如 `context_compaction`。
- `evidence_log_seq_range` 用于关联实时日志，避免保存大段重复日志。
- 第一阶段只保存结构化元数据，不保存完整 prompt 或完整 answer。

---

## 7. 时序字段定义

### 7.1 `request_started_at`

普通代理请求进入 `proxyRequest()` 的时间。

### 7.2 `upstream_fetch_started_at`

调用上游 `fetch()` 前的时间。

### 7.3 `upstream_headers_at`

收到上游响应头的时间。

非流式：

- 表示上游已经返回响应头，但响应体可能还未完全读取。

流式：

- 表示 SSE 连接建立。

### 7.4 `first_stream_chunk_at`

流式场景下收到第一个 chunk 的时间。

非流式为空。

### 7.5 `first_content_at`

第一次观测到有效内容事件的时间。

优先识别：

- `response.output_text.delta`
- `message.delta`
- Chat Completions delta content
- 可视 answer 文本片段

如果只有 reasoning / metadata / heartbeat，不算 first content。

### 7.6 `final_chunk_at`

流式场景下收到最后一个有效 chunk 的时间。

### 7.7 `request_finished_at`

网关完成当前尝试的时间。

注意：

- 如果命中规则后内部重试，每一次请求尝试都应形成独立样本。
- 最终对 Codex 返回的那一次也要能从样本里看出来。

---

## 8. 耗时派生指标

### 8.1 `duration_total_ms`

```text
request_finished_at - request_started_at
```

表示网关视角总耗时。

不能单独用于异常判断。

### 8.2 `upstream_wait_ms`

```text
upstream_headers_at - upstream_fetch_started_at
```

表示等待上游响应头的时间。

### 8.3 `time_to_first_chunk_ms`

```text
first_stream_chunk_at - upstream_fetch_started_at
```

只用于流式。

### 8.4 `time_to_first_content_ms`

```text
first_content_at - upstream_fetch_started_at
```

比 first chunk 更接近用户体感。

### 8.5 `stream_duration_ms`

```text
final_chunk_at - first_stream_chunk_at
```

只用于流式。

---

## 9. TPS 指标定义

TPS 指标必须明确分母，避免误读。

### 9.1 `output_tps`

```text
output_tokens / (duration_total_ms / 1000)
```

表示从请求开始到结束的整体可见输出速度。

缺点：

- 混入了上游等待时间。

### 9.2 `visible_output_tps`

```text
output_tokens / (stream_duration_ms / 1000)
```

只用于流式。

表示真正开始流式输出后的可见输出速度。

### 9.3 `total_observed_tps`

```text
(reasoning_tokens + output_tokens) / (duration_total_ms / 1000)
```

表示包括隐藏 reasoning token 后的总 token 速度近似。

注意：

- reasoning tokens 不一定按真实生成时间线均匀产生。
- 这个值只能做横向比较，不能当绝对真值。

### 9.4 `reasoning_adjusted_tps`

```text
(reasoning_tokens + output_tokens) / ((request_finished_at - upstream_headers_at) / 1000)
```

用于减少上游排队时间影响。

注意：

- 如果 `upstream_headers_at` 与 `request_finished_at` 太近，必须避免除以极小值导致离谱数值。
- 建议分母小于 `250ms` 时标记为 `insufficient_duration`。

### 9.5 时序归一化偏差

耗时、TPS、token 长度不能拆开单看。第一阶段不预设唯一公式，而是在统计层把它们归并为一个复合特征，暂命名为 `时序归一化偏差`。

这个指标应满足：

- 输入原始材料来自 `duration_total_ms`、`output_tokens` / `total_tokens`、`output_tps`、`visible_output_tps`、`reasoning_adjusted_tps`
- 用于比较“同等 token 规模、同等模型家族、同等思考等级”下的相对速度残差
- UI 展示时作为一个整体出现，不拆成多个独立特征
- 原始耗时、TPS、token 长度仍保留在样本明细和导出里，供人工复盘

---

## 10. 响应结构特征

第一阶段重点统计下面结构信号：

```js
{
  has_commentary,
  has_final_answer,
  final_answer_only,
  has_tool_call,
  has_reasoning_item,
  has_output_text,
  event_type_counts,
  response_item_type_counts
}
```

### 10.1 `final_answer_only`（仅最终答案结构）

定义：

- 有 final answer
- 没有 commentary
- 没有 tool call
- 没有可观测 reasoning item
- 没有其他可观测中间阶段

该字段只能表示“可观测结构形态”，不直接表示异常，也不证明模型内部没有思考。

### 10.2 `commentary_observed`（观测到 commentary 信号）

`commentary_observed` 是 `has_commentary` 的对外语义别名，只表示响应事件或 item 中观测到 commentary 相关信号。`has_commentary` 继续保留用于兼容旧样本和旧导出脚本。

注意：

- 不要求也不可能拿到隐藏思维链文本。
- 不尝试绕过模型的隐藏 reasoning 机制。
- `commentary_observed=false` 只能表示“未观测到 commentary 信号”，不证明模型内部没有 commentary。

### 10.3 `event_type_counts`

用于统计流式事件形态，例如：

```js
{
  "response.created": 1,
  "response.output_item.added": 2,
  "response.output_text.delta": 34,
  "response.completed": 1
}
```

### 10.4 `response_item_type_counts`

用于统计 Responses output item 形态，例如：

```js
{
  "reasoning": 1,
  "message": 1,
  "function_call": 0
}
```

---

## 11. 聚合统计设计

状态接口新增一个 reasoning 行为分析快照，建议命名：

```json
{
  "reasoning_behavior": {}
}
```

### 11.1 总览统计

```js
{
  total_samples,
  streaming_samples,
  non_streaming_samples,
  by_model_family,
  by_reasoning_effort,
  by_model_family_and_effort,
  final_answer_only_count,
  commentary_observed_count,
  commentary_present_count, // legacy alias
  tool_call_count,
  matched_rule_count,
  blocked_count
}
```

### 11.2 reasoning token 分布

```js
{
  reasoning_token_counts: {
    "0": 31,
    "516": 3,
    "765": 1
  },
  top_reasoning_tokens: [
    { value: 516, count: 3, ratio: 0.04 }
  ]
}
```

### 11.3 结构关联统计

按 `reasoning_tokens` 分组：

```js
{
  by_reasoning_token: [
    {
      value: 516,
      count: 3,
      final_answer_only_count: 3,
      final_answer_only_ratio: 1,
      commentary_observed_count: 0,
      commentary_observed_ratio: 0,
      commentary_present_count: 0, // legacy alias
      commentary_present_ratio: 0, // legacy alias
      avg_total_tokens: 180,
      avg_duration_total_ms: 12000,
      avg_time_normalization_deviation: 0.82
    }
  ]
}
```

### 11.4 时序归一化统计

第一阶段先做固定 bucket：

```js
{
  time_normalization_deviation_buckets: [
    { label: "低", count: 0 },
    { label: "中", count: 0 },
    { label: "高", count: 0 }
  ],
  raw_tps_buckets: []
}
```

后续再根据真实数据调整 bucket。

### 11.5 候选特征统计

第一阶段只展示候选，不进入拦截。

候选组合示例：

```js
{
  candidate_patterns: [
    {
      pattern_key: "reasoning_tokens=516|final_answer_only|commentary_not_observed|time_normalization_deviation",
      count: 3,
      ratio: 0.04,
      avg_total_tokens: 180,
      avg_duration_total_ms: 12000,
      avg_time_normalization_deviation: 0.82,
      last_seen_at: "2026-06-29T08:59:19.004Z"
    }
  ]
}
```

候选特征必须满足：

- 出现次数大于等于配置阈值，默认 `3`
- 或者最近连续出现

但 UI 只显示为候选，不自动启用规则。

当前第一版特征定位基线先按这条组合观察，不拆单项：

- `reasoning_tokens` 异常值
- `final_answer only`
- `commentary_not_observed`
- 时序归一化偏差

---

## 12. UI 大盘设计

新增管理页区块：

- `reasoning 行为统计`

### 12.1 概览卡片

展示：

- 样本总数
- reasoning_tokens=516 观察占比（非定性）
- final_answer only 占比
- commentary observed 占比
- 普通观察 516 与候选复盘 516 的分布（仅观察）
- gpt-5.4 / gpt-5.5 分布
- reasoning.effort 分布
- 当前规则命中样本
- 实际拦截样本
- 平均总耗时
- 平均 token 长度
- 平均时序归一化偏差

### 12.2 图表

第一阶段建议用无依赖实现。

可用 CSS / HTML 简单柱状图，避免引入前端构建链。

图表：

1. `reasoning_tokens 高频值柱状图`
2. `reasoning_tokens=516 与 final_answer only 占比`
3. `reasoning_tokens=516 与 commentary observed 占比`
4. `模型 family × 思考等级矩阵`
5. `时序归一化偏差分布`

### 12.3 表格

表格一：高频 reasoning token 值

列：

- reasoning_tokens
- 次数
- 占比
- final_answer only 占比
- commentary observed 占比
- 主力模型 family
- 主力思考等级
- 平均总耗时
- 平均 token 长度
- 平均时序归一化偏差
- 最近出现时间

表格二：候选特征案例

列：

- 特征案例
- 次数
- 占比
- 触发 reasoning_tokens
- 主力模型 family
- 主力思考等级
- 平均总耗时
- 平均 token 长度
- 平均时序归一化偏差
- 最近出现时间
- 当前状态：仅观察

表格三：最近样本

列：

- request_id
- 时间
- 路径
- 模型
- 模型家族
- 思考等级
- reasoning_tokens
- output_tokens
- total_tokens
- duration_total_ms
- 上游模型
- final_answer only
- commentary
- 时序归一化偏差
- 命中规则
- 实际拦截
- 日志证据

### 12.4 设置导出按钮

在管理页“设置”或“工具”区域新增一个导出按钮，方便把当前统计数据和样本导出给外部分析。

导出内容建议：

- reasoning token 分布
- final_answer only / commentary observed 统计
- 模型、模型家族、`reasoning.effort`
- 请求/响应时间与时序归一化偏差
- 候选特征案例
- 最近样本摘要
- 当前配置快照

导出格式建议：

- 默认导出为 `JSON`
- 额外提供 `CSV` 便于表格软件查看

导出按钮行为：

- 只导出结构化统计，不导出完整 prompt 或完整 answer
- 支持选择开始日期和结束日期
- `31` 天以内同步读取选中日期范围内的日文件并合并
- `32` 天及以上创建后台导出任务，按日期分段处理，页面轮询进度并提示不影响 gateway 正常使用
- 导出文件名带日期范围和时间戳
- 导出失败要有明确错误提示
- 完成后提供下载链接；后续如果数据量继续增大，再补压缩包、每日 rollup 或样本索引

---

## 13. 持久化设计

第一阶段不要只放内存，否则无法“随时统计”。

当前预估一天只有几千条请求，没有必要引入数据库。

采用“每日一个 JSON 文件”的方案：

```text
<state_root>/analytics/reasoning-behavior-YYYY-MM-DD.json
```

每个文件保存当天样本与可选聚合摘要。

推荐文件结构：

```json
{
  "date": "2026-06-29",
  "schema_version": 2,
  "generated_by": "codex-retry-gateway",
  "samples": [],
  "daily_summary": {}
}
```

### 13.1 写入策略

- 每个被检查响应尝试结束后写入当天文件。
- 运行期可以先在内存累积当天样本，再按节流策略落盘。
- 推荐写入策略：
  - 样本进入内存最近窗口
  - 当天样本进入内存 daily buffer
  - 每隔固定时间或样本数阈值写一次日文件
  - 进程退出前尽量 flush
- 写入失败只记日志，不影响代理主链路。

### 13.2 内存窗口

内存只保留最近 N 条：

```json
{
  "analytics_recent_sample_limit": 500
}
```

用于 UI 快速展示。

### 13.3 日文件命名

按本机日期生成文件名：

```text
reasoning-behavior-2026-06-29.json
```

注意：

- 文件日期按本机本地日期切分，而不是 UTC 日期。
- UI 展示和导出也按本地日期选择。
- 每个日文件只保存当天样本，便于人工收集和分发。

### 13.4 导出合并

导出时不需要数据库查询。短时间段可以同步导出，大时间段应拆成后台任务。

同步流程：

1. 用户在 UI 选择开始日期和结束日期。
2. 范围在 `31` 天以内时，网关读取日期范围内所有存在的日文件。
3. 合并 `samples`。
4. 重新计算导出范围内的聚合统计。
5. 导出为 JSON 或 CSV。

后台流程：

1. 范围达到 `32` 天及以上时，导出接口返回 HTTP `202` 和 `export_job.job_id`。
2. 后台任务按本地日期逐日读取日文件和当前内存缓冲。
3. 每处理一天更新 `processed_days`、`sample_count` 和 `percent`，并让出事件循环，避免影响普通代理请求。
4. 前端轮询任务状态接口，展示进度条和“不影响正常使用”的提醒。
5. 任务完成后写入 exports 目录，并返回下载链接。

导出 JSON 推荐结构：

```json
{
  "exported_at": "2026-06-29T12:00:00.000Z",
  "date_from": "2026-06-29",
  "date_to": "2026-06-30",
  "schema_version": 2,
  "summary": {},
  "samples": []
}
```

### 13.5 隐私边界

日文件和导出文件都不保存：

- 原始 prompt 全文
- 原始 answer 全文
- Authorization header
- 请求体完整内容
- 响应体完整内容
- 未脱敏的原始日志全文
- 非必要的二进制内容

只保存结构化统计字段，但字段要尽可能详细，至少包含：

- 请求模型、模型家族、`reasoning.effort`
- 请求/响应时间戳
- token、耗时、TPS、结构信号
- 命中规则、是否拦截、是否重试、是否失败
- 上游模型、状态码、错误摘要、日志证据范围

### 13.6 海量数据分析与分层治理

全量采集不等于每次大盘刷新都全量深解析。后续数据量变大后，分析口径必须分层，避免被历史日志拖死，也避免因为只看抽样导致误判。

分层原则：

1. `gateway analytics` 是未来逐请求事实源。
2. `CC Switch` 本地日志和 `Codex session` 历史日志只用于历史回填、字段探索和交叉校验，不替代未来实时事实源。
3. 大盘优先读聚合结果，只有下钻时才读明细样本。
4. 导出优先按时间段、模型家族、思考等级、候选特征过滤，避免无边界导出。
5. 任何候选特征都必须能反查到样本明细和日志证据范围。

推荐数据层：

```text
请求尝试样本 -> 当日 JSON 明细 -> 当日 rollup -> 时间段 rollup -> 下钻样本导出
```

各层职责：

- 请求尝试样本：记录每次上游尝试、内部重试、最终拦截、失败和旁路请求的结构化事实。
- 当日 JSON 明细：保留当天完整结构化样本，文件名按本地日期切分。
- 当日 rollup：按模型、模型家族、`reasoning.effort`、状态、`reasoning_tokens`、结构形态、时序归一化偏差做聚合。
- 时间段 rollup：UI 选择时间段后合并多个日文件的 rollup 或样本，重新计算统计。
- 下钻样本导出：只在用户需要复盘时导出明细，默认不把所有历史样本一次性塞进大盘。

### 13.7 数据源分工

三类数据源要分开看：

1. `gateway analytics`
   - 作为后续主事实源。
   - 覆盖所有经过本 gateway 的请求尝试，包括重试请求、拦截请求、失败请求和旁路请求。
   - 字段围绕模型、模型家族、`reasoning.effort`、token、耗时、TPS、结构信号、状态、重试链路和证据范围设计。
2. `CC Switch` 本地日志
   - 用于确认本机路由链路、provider、历史请求字段和上游返回形态。
   - 适合导出字段清单和代表样本，不适合直接承担大盘实时查询。
3. `Codex session` 历史日志
   - 用于回看 Codex 客户端侧的会话状态、token_count、工具调用和异常收口现象。
   - 历史体量可能达到 20GB 级别，不能用单进程全量 JSON 深解析作为常规分析方式。

历史日志分析策略：

- 先用 `rg` / SQLite schema / key 扫描做字段定位。
- 再选最近、关键、可疑的代表文件做 JSON 深解析。
- 输出字段清单、字段覆盖率和样本摘要。
- 不把历史日志全量解析结果直接并入实时大盘，除非后续做了可断点续扫的增量索引。

### 13.8 大数据量下的查询与导出口径

时间段大盘必须能选开始日期和结束日期，但实现上要遵守下面约束：

- 默认展示最近窗口和轻量聚合，不默认读取所有历史日文件。
- 选择时间段时，只读取命中的日文件。
- 如果时间段很大，优先返回聚合统计，再提供 CSV / JSON 导出做离线分析。
- 明细表只展示最近 N 条或当前筛选后的前 N 条。
- 导出文件要带 `date_from`、`date_to`、`schema_version` 和 `exported_at`。
- CSV 适合表格分析，JSON 适合保留完整结构和候选特征上下文。

硬降级阈值建议：

- 默认 UI 明细窗口最多展示 `500` 条最近样本。
- 单次时间段 UI 下钻最多直接读取 `7` 个日文件；超过后先返回 rollup，并提示缩小时间段或走离线导出。
- 单次明细渲染最多 `1000` 条样本；超过后只展示聚合、top 列表和候选特征摘要。
- 单个日文件超过 `50MB` 后，UI 优先读取每日 rollup；超过 `200MB` 后禁止 UI 直接全量读取明细。
- JSON / CSV 同步导出限制在 `31` 天以内；`32` 天及以上进入后台分段导出任务。
- 任意查询超过 `3s` 应返回可解释的降级提示，不允许让管理页无限等待。

后续如果日文件增长到明显影响 UI 响应，再升级为：

- 每日 rollup 文件：只存聚合统计。
- 明细索引文件：只存 `sample_id`、时间、模型、token、状态、候选特征 key。
- 后台分段导出：当前已支持按日期慢慢导出并轮询进度。
- 压缩包导出：后续把 JSON / CSV / 字段说明一起打包。

### 13.8.1 历史导入分析模块

历史导入分析是独立模块，不属于实时 reasoning analytics 主链路。它用于分析本机已经存在的大体量历史数据，但不能拖慢管理页，也不能影响当前 gateway 代理路由。

本模块后续必须改成“先验价值筛选，再导入分析”：

- 历史数据不是“能扫多少算多少”。
- 只有具备 reasoning 行为分析特征的数据源，才允许进入分析大盘。
- 如果数据源缺少核心特征字段，导入任务应尽早停止深解析，只输出轻量 preflight 结果。
- 对无价值数据源，UI 显示“无分析价值：缺少 reasoning 行为特征字段”，不进入候选特征分析。

第一版导入范围：

1. `CC Switch SQLite`
   - 默认路径：`%USERPROFILE%\.cc-switch\cc-switch.db`
   - 关键表：`proxy_request_logs`
   - 聚合字段：模型、请求模型、provider、状态码、输入 token、输出 token、cache token、耗时、首 token 延迟、streaming、创建时间、成本字段。
2. `Codex logs SQLite`
   - 默认路径：`%USERPROFILE%\.codex\sqlite\logs_2.sqlite` 和 `%USERPROFILE%\.codex\logs_2.sqlite`
   - 关键表：`logs`
   - 聚合字段：level、target、日志行数、关键词命中，例如 `reasoning_tokens`、`final_answer`、`commentary`、`502`。
3. `Codex sessions JSONL`
   - 默认路径：`%USERPROFILE%\.codex\sessions`
   - 第一版只做文件级索引：文件数、总体积、top 大文件、修改时间。
   - 不做完整 JSONL 深解析，避免 20GB 级历史会话把 UI 或 gateway 主进程拖死。

### 13.8.2 历史导入 Preflight 价值筛选

历史导入任务必须先执行 preflight。preflight 只做轻量 schema / 字段 / 少量样本扫描，用来判断数据是否具备分析价值。

核心字段门槛：

1. `reasoning_tokens`
2. `final_answer_only`，或足够的响应结构字段，可以推导 `final_answer_only`
3. `commentary_observed`，或足够的事件 / item 结构字段，可以推导 `commentary_observed`
4. `duration_total_ms` 或可推导的请求开始 / 结束时间
5. `output_tokens` / `total_tokens`，用于 TPS 与时序归一化
6. `model` / `model_family`
7. `reasoning.effort`
8. 请求状态、重试状态、拦截状态

价值等级：

- `valuable`：核心字段足够，可以进入特征分析大盘。
- `partial`：只缺少少量辅助字段，可以进入覆盖率大盘，但不能给出强候选结论。
- `no_analysis_value`：缺少 `reasoning_tokens`、`final_answer_only` 或 `commentary_observed` 这类核心字段，放弃深导入。

严格规则：

- 缺少 `reasoning_tokens` 时，不进入 reasoning 特征分析。
- 缺少 `final_answer_only` 且无法从结构推导时，不进入候选特征分析。
- 缺少 `commentary_observed` 且无法从结构推导时，不进入候选特征分析。
- 只有普通文本日志、会话正文、模型名、时间戳，但不能恢复上述结构信号时，视为 `no_analysis_value`。
- `Codex sessions JSONL` 默认只做文件级索引；除非 preflight 证明存在可解析结构字段，否则不做正文级深解析。

preflight 输出字段：

```json
{
  "analysis_value": "valuable | partial | no_analysis_value",
  "can_build_reasoning_features": true,
  "can_build_candidate_patterns": true,
  "field_coverage": {
    "reasoning_tokens": 0.98,
    "final_answer_only": 0.94,
    "commentary_observed": 0.94,
    "duration_total_ms": 0.91,
    "output_tokens": 0.89,
    "model_family": 1,
    "reasoning_effort": 0.76
  },
  "missing_core_fields": [],
  "decision_reason": "核心字段覆盖率足够，可以进入特征分析"
}
```

UI 要求：

- 历史导入按钮文案从“开始导入分析”升级为“预检并分析”。
- preflight 失败时，不展示大盘图表，只展示缺失字段、样本覆盖率和放弃原因。
- preflight 通过时，才显示“生成特征分析大盘”。
- 历史导入结果不应把“扫到多少文件”作为核心成果；核心成果是“是否具备分析价值，以及能否支持候选特征确认”。

历史导入 API：

```http
POST /__codex_retry_gateway/api/analytics/imports/run
GET /__codex_retry_gateway/api/analytics/imports/jobs/<job_id>
GET /__codex_retry_gateway/api/analytics/imports/latest
```

实现约束：

- `POST /run` 只创建后台任务并返回 `202`，普通代理请求不等待导入完成。
- UI 只轮询任务状态和摘要，不把历史明细一次性渲染到页面。
- 聚合结果写入 `<state_root>/analytics/imports/<job_id>/summary.json`。
- 如果请求体传入 `source_paths`，只分析指定数据源，不混入默认真实大库，方便分段导入、测试和避免误扫。
- SQLite 只执行聚合 SQL，不导出完整 prompt、完整 answer、Authorization、Cookie 或未脱敏正文。
- session JSONL 第一版只做文件级索引；后续如需正文级特征，必须先做可断点续扫的增量索引和脱敏策略。
- preflight 判定为 `no_analysis_value` 时，任务可以直接完成，不继续深导入。
- preflight 判定为 `partial` 时，任务可以保存字段覆盖率报告，但不能输出“候选特征确认”。

### 13.9 特征定位分析流程

516 的分析不能再停留在“命中即异常”。第一阶段的分析流程固定为：

1. 先按模型家族区分 `gpt-5.4` / `gpt-5.5`。
2. 再按 `reasoning.effort` 区分思考等级。
3. 再按 token 规模分层，至少区分输入 token、输出 token、reasoning token 和 total token。
4. 在同一模型家族、同一思考等级、相近 token 规模内比较时序归一化偏差。
5. 最后叠加结构特征：`final_answer only`、`commentary_not_observed`、无工具调用、响应 item / event 分布。

候选复盘 516 的第一版定义：

```text
reasoning_tokens=516
+ final_answer only
+ commentary_not_observed
+ 时序归一化偏差高
```

普通观察 516 的定义：

```text
reasoning_tokens=516
+ 未同时满足候选复盘组合
```

注意：

- “普通观察 516”也不是“确认正常”，只是当前证据不足以进入候选复盘组合。
- “候选复盘 516”也不是“确认降智”，只是进入重点复盘队列。
- 耗时、TPS、token 长度只作为同一个复合归一化维度出现，不拆成单独判据。

### 13.9.1 统一分析 Profile

实时 reasoning 行为统计和历史导入分析必须共用同一套分析 Profile。区别只在数据源，不在判定口径。

默认 Profile：

```json
{
  "name": "516_candidate_review_v1",
  "data_source": "runtime | historical_import",
  "filters": {
    "date_from": null,
    "date_to": null,
    "model_family": ["gpt-5.4", "gpt-5.5"],
    "reasoning_effort": ["low", "medium", "high", "xhigh"],
    "streaming": "any",
    "status": "any",
    "include_retries": true,
    "include_blocked": true
  },
  "conditions": {
    "reasoning_tokens": [516],
    "reasoning_tokens_mode": "equals_or_outlier",
    "final_answer_only": true,
    "commentary_not_observed": true,
    "time_normalization_deviation": "high"
  },
  "baseline": {
    "group_by": ["model_family", "reasoning_effort", "token_scale_bucket"],
    "compare_with_non_candidate_samples": true
  }
}
```

分析条件 UI：

- 时间段
- 数据源：实时 analytics / 历史导入
- 模型家族：`gpt-5.4` / `gpt-5.5` / 其他
- 模型名
- `reasoning.effort`
- `reasoning_tokens`：516 / 0 / 自定义 / outlier
- `final_answer_only`：任意 / 是 / 否
- `commentary_observed`：任意 / observed / not observed
- 状态：成功 / 拦截 / 上游失败 / gateway 拒绝
- 是否包含 gateway 内部重试
- token 规模分桶

### 13.9.2 特征确认大盘

分析大盘不是导出页，而是判断“数据是否具备我们的 reasoning 行为特征”的工作台。

大盘必须展示：

1. `analysis_value`
   - 当前数据源是否有分析价值。
   - 字段不足时直接标记为 `no_analysis_value` 或 `partial`。
2. 字段覆盖率
   - `reasoning_tokens`
   - `final_answer_only`
   - `commentary_observed`
   - `duration_total_ms`
   - `output_tokens`
   - `model_family`
   - `reasoning.effort`
3. 特征命中
   - 命中样本数
   - 命中占比
   - 最近出现时间
   - 是否集中在 `516`
4. 基线对比
   - 同模型家族、同思考等级、相近 token 规模下的普通样本分布
   - 候选样本相对普通样本的 `final_answer_only`、`commentary_not_observed`、时序归一化偏差差异
5. 结论等级
   - `no_analysis_value`
   - `insufficient_fields`
   - `not_observed`
   - `candidate`
   - `strong_candidate`
   - `high_false_positive_risk`
6. 明细下钻
   - 只展示脱敏结构字段、数值字段和状态字段。
   - 不展示完整 prompt / answer / Authorization / Cookie。

结论规则：

- `no_analysis_value`：核心字段缺失，放弃导入或放弃分析。
- `insufficient_fields`：字段覆盖率不足，只能展示覆盖率，不能判断特征。
- `not_observed`：字段足够，但未观察到目标组合。
- `candidate`：目标组合出现，但样本量或区分度不足。
- `strong_candidate`：目标组合在同模型、同 effort、同 token 规模下明显高于基线。
- `high_false_positive_risk`：目标组合出现，但同类正常样本也大量出现，不能用于拦截。

UI 文案要求：

- 首屏必须写明“516 只是观察点，不代表降智结论”。
- 所有候选组合都标记为“仅观察 / 候选复盘”，不能标记为“已命中异常”。
- `时序归一化偏差` 必须标注为“复合相对残差”，不能展示成单独告警阈值。
- `gpt-5.4` / `gpt-5.5` 展示为模型版本或模型族维度，`reasoning.effort` 单独展示为思考等级维度，不能混成一个字段。

---

## 14. API 设计

状态接口可以先返回最近窗口统计：

```http
GET /__codex_retry_gateway/api/status
```

新增：

```json
{
  "reasoning_behavior": {
    "summary": {},
    "top_reasoning_tokens": [],
    "by_reasoning_token": [],
    "tps_buckets": {},
    "candidate_patterns": [],
    "recent_samples": []
  }
}
```

后续如果 UI 查询变重，再拆独立接口：

```http
GET /__codex_retry_gateway/api/analytics/reasoning
```

当前实现已经拆出独立观测接口，并支持按本地日期筛选：

```http
GET /__codex_retry_gateway/api/analytics/reasoning?date_from=2026-06-29&date_to=2026-06-30
```

返回内容：

- `summary`
- `top_reasoning_tokens`
- `by_reasoning_token`
- `by_model_family`
- `by_reasoning_effort`
- `by_model_family_and_effort`
- `candidate_patterns`
- `recent_samples`

后续新增特征分析接口：

```http
POST /__codex_retry_gateway/api/analytics/reasoning/analyze
POST /__codex_retry_gateway/api/analytics/imports/analyze
```

`/reasoning/analyze` 使用实时 analytics 日文件和内存缓冲作为数据源。

`/imports/analyze` 使用最近一次或指定 `job_id` 的历史导入结果作为数据源；如果历史导入 preflight 结果为 `no_analysis_value`，接口直接返回 `analysis_value=no_analysis_value`，不做深度分析。

统一响应结构：

```json
{
  "ok": true,
  "analysis_profile": "516_candidate_review_v1",
  "analysis_value": "valuable | partial | no_analysis_value",
  "conclusion": "no_analysis_value | insufficient_fields | not_observed | candidate | strong_candidate | high_false_positive_risk",
  "field_coverage": {},
  "candidate_summary": {},
  "baseline_comparison": {},
  "samples_preview": []
}
```

接口约束：

- `samples_preview` 只允许返回脱敏结构字段、数值字段和状态字段。
- 历史导入字段不足时，不为了凑分析结果去解析完整会话正文。
- 分析接口只输出观察和候选结论，不修改现有拦截规则。

导出接口：

```http
GET /__codex_retry_gateway/api/analytics/reasoning/export?format=json&date_from=2026-06-29&date_to=2026-06-30
GET /__codex_retry_gateway/api/analytics/reasoning/export?format=csv&date_from=2026-06-29&date_to=2026-06-30
```

导出说明：

- `format=json` 保留结构化样本和聚合上下文。
- `format=csv` 面向表格分析，重点展开核心字段。
- 不传时间段时，导出可读取已有日文件和当前内存缓冲。
- 时间段按本地日期 `YYYY-MM-DD` 过滤。
- `31` 天以内同步返回导出文件。
- `32` 天及以上返回 HTTP `202`、`background_export=true` 和 `export_job.job_id`，由前端轮询任务进度。
- 后台任务完成后通过 `/__codex_retry_gateway/api/analytics/reasoning/export/jobs/<job_id>/download` 下载。
- 导出不包含完整 prompt、完整 answer 或 Authorization。

导出脱敏与截断要求：

- `request_payload_excerpt` 只允许作为排查辅助字段，必须截断，当前建议上限 `500` 字符。
- `failure_summary.message`、`response_summary`、错误摘要等文本字段必须截断，建议上限 `320` 字符。
- Authorization、Cookie、Set-Cookie、完整请求体、完整响应体永不导出。
- 请求摘要优先保存 `body_bytes`、`body_sha256`、脱敏 header 和结构化参数，不把 prompt/answer 当正文保存。
- 如果后续发现部分请求预览仍可能包含敏感业务文本，导出侧必须提供关闭预览字段的选项，默认导出可只保留 hash 和长度。
- CSV 导出要比 JSON 更保守，优先导出结构字段、数值字段和状态字段。

---

## 15. 与现有拦截逻辑的关系

第一阶段：

- 现有 `reasoning_equals` 继续生效。
- 现有 `guard_retry_attempts` 继续生效。
- 现有统计 `matched_response_count` / `blocked_response_count` 继续生效。
- 新增行为统计只旁路观察，不改变响应，也不把新特征直接并入拦截面。

后续阶段才考虑：

- 从候选特征生成 observe-only 规则
- 由用户确认后升级为拦截规则
- 多特征组合匹配
- 先让普通观察 516 与候选复盘 516 的差异被稳定定位出来，再决定是否收窄现有全拦策略

---

## 16. 后续特征规则方向

未来规则不再写成：

```json
{
  "reasoning_equals": [516]
}
```

而是逐步演进为：

```json
{
  "reasoning_tokens_outlier": [516],
  "final_answer_only": true,
  "commentary_not_observed": true,
  "time_normalization_deviation": "high",
  "mode": "observe"
}
```

或：

```json
{
  "pattern": "reasoning_outlier_short_circuit",
  "conditions": {
    "reasoning_tokens_outlier": [516],
    "final_answer_only": true,
    "commentary_not_observed": true,
    "time_normalization_deviation": "high"
  },
  "mode": "intercept"
}
```

注意：

- 第一阶段不实现这类规则。
- 先让统计大盘跑出真实分布。

---

## 17. 测试设计

第一阶段实现时至少覆盖：

1. 流式请求尝试应记录 reasoning 行为样本。
2. 非流式请求尝试应记录 reasoning 行为样本。
3. 样本应记录 `reasoning_tokens`、输出 token、时序字段与结构字段。
4. `final_answer only` 样本应正确计数。
5. 有 commentary 的样本不应计为 `final_answer only`。
6. `output_tps` 与 `reasoning_adjusted_tps` 应按定义计算。
7. 分母过小时应标记为不可用，不应产生无穷大或离谱值。
8. 命中现有规则时，样本应记录 `matched_current_rule = true`。
9. 网关内重试样本应记录 `attempt_id` 信息。
10. 日文件写入失败不应影响代理请求。
11. UI 应展示 reasoning 行为统计区块。
12. UI 图表在无样本时应显示空状态，不应报错。
13. 导出按钮应能导出 JSON 统计文件。
14. 导出内容不应包含原始 prompt、完整 answer 或 Authorization header。
15. CSV 导出应包含核心聚合字段，便于外部表格分析。
16. 按日期范围导出时，应能合并多个日文件并重新计算聚合统计。
17. 超过 `31` 天的大范围导出应返回 `202` 后台任务，而不是阻塞请求或返回 `413`。
18. UI 应展示后台导出进度、继续使用提醒和完成后的下载链接。
19. 历史导入 preflight 缺核心字段时，应返回 `analysis_value=no_analysis_value`，并停止深导入。
20. 历史导入 preflight 通过时，才允许生成特征分析大盘。
21. reasoning 行为统计分析条件应支持时间、模型家族、模型、`reasoning.effort`、`reasoning_tokens`、`final_answer_only`、`commentary_observed`、状态、重试和拦截过滤。
22. 分析结论必须区分 `no_analysis_value`、`insufficient_fields`、`not_observed`、`candidate`、`strong_candidate`、`high_false_positive_risk`。

---

## 18. 实施阶段建议

### 阶段 1：内存样本与状态接口

只做：

- 样本结构
- 内存最近窗口
- 聚合统计
- E2E 覆盖

不做：

- 文件持久化
- UI 图表

### 阶段 2：按日 JSON 持久化

只做：

- 每日一个 JSON 文件
- 按本地日期切分
- 启动时可选择加载最近 N 条

### 阶段 3：UI 大盘

只做：

- 概览卡片
- 高频 reasoning token 表
- 简单柱状图
- 最近样本表
- 设置区导出按钮
- JSON 导出
- CSV 导出

### 阶段 4：候选特征观察

只做：

- candidate pattern 聚合
- UI 标记“仅观察”
- 不接入拦截

### 阶段 5：特征规则

在数据足够后再做：

- observe-only 特征规则
- 用户手动升级为 intercept
- 多特征组合拦截

### 18.1 当前实现状态与激活边界

截至本轮方案补全，代码层已经具备第一阶段所需的核心能力：

- 全量请求尝试样本采集入口。
- gateway 内部重试样本记录。
- 旁路请求、失败请求、本地拒绝请求样本记录。
- 按日 JSON 落盘。
- 最近窗口大盘。
- 时间段观测接口。
- JSON / CSV 导出接口。
- 大范围后台导出任务、进度轮询和下载接口。
- 候选特征组合聚合。

但运行态必须单独确认：

- 如果 `127.0.0.1:4610` 仍是旧进程，它不会自动拥有新接口和新落盘能力。
- 只有重启或重新拉起 gateway 后，新代码才会开始写入 `<state_root>/analytics/reasoning-behavior-YYYY-MM-DD.json`。
- 验证 `GET /__codex_retry_gateway/api/analytics/reasoning` 必须返回 JSON，且包含 `ok: true` 与 `schema_version: 2`；如果返回上游 HTML、普通代理内容或缺少 schema 版本，说明当前进程没有加载新版本或 analytics 未完整初始化。
- 不应为了验证而直接杀掉当前正在承载 Codex 会话的路由进程，除非用户明确批准。

后续建议增加机器可判定硬信号：

- `analytics_ready: true`
- `analytics_schema_version: 2`
- `analytics_started_at`
- `analytics_state_root`
- 最近一次 flush 结果与错误摘要

---

## 19. 风险与边界

1. reasoning token 数是上游报告值，仍可能被上游错误或伪造。
2. TPS 只能作为统计参考，不能作为独立真值。
3. `final_answer only` 只是响应结构，不代表一定异常。
4. `commentary_not_observed` 可能是正常简单任务结果，只表示未观测到 commentary 信号。
5. 日文件持久化不能保存敏感正文。
6. 图表必须服务排查，不能让 UI 变成误导性“定罪面板”。

---

## 20. 最终决定

本项目后续从“固定 516 拦截器”演进为“reasoning 行为观测与特征匹配网关”。

第一阶段先落地：

- 数据统计
- 全量详细采集
- 时序拆分
- 时序归一化
- 响应结构统计
- reasoning token 分布
- 大盘展示
- 516 现有拦截逻辑保留

其中，特征定位的第一版基线先按：

- `reasoning_tokens` 异常值
- `final_answer only`
- `commentary_not_observed`
- 时序归一化偏差

来做组合观察，不把 `516` 单独写成唯一特征结论。

暂不落地：

- 自动降智判断
- 新特征拦截规则
- 模型真实身份归因
- 现有功能面调整
- 现有拦截配置变更

最终判断链路必须是：

```text
采样 -> 全量详细采集 -> 统计 -> 发现候选特征案例 -> observe-only -> 人工确认 -> 进入特征拦截规则
```
