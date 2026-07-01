# reasoning 行为观测大盘实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不干预现有路由主链和拦截主流程的前提下，补齐 reasoning 行为观测大盘的时间段选择、聚合展示、科技感 UI 和回归测试。

**Architecture:** 继续复用 `gateway.mjs` 里现有的 reasoning 样本采集、日文件落盘和聚合逻辑，只新增时间段查询入口与前端筛选状态。UI 仍然挂在管理页内，但数据读取改为支持 `date_from` / `date_to` 的观测视图，默认仍展示最近样本窗口，时间段选择只影响观测面和导出面。

**Tech Stack:** Node.js, 原生 HTTP, 现有管理页内联 HTML/CSS/JS, 现有 E2E 测试脚本

---

### Task 1: 让 reasoning 观测接口支持时间段

**Files:**
- Modify: `gateway.mjs`
- Test: `scripts/test-gateway-e2e.mjs`

- [ ] **Step 1: 先补时间段接口断言**

```js
const rangeResponse = await fetch(`${base}/__codex_retry_gateway/api/analytics/reasoning?date_from=2026-06-29&date_to=2026-06-30`);
const rangePayload = await rangeResponse.json();
assert(rangeResponse.ok, "时间段 reasoning 接口应返回成功");
assert(Array.isArray(rangePayload.recent_samples), "时间段 reasoning 接口应返回样本");
```

- [ ] **Step 2: 让测试先失败，确认接口还不支持时间段查询**

Run: `node .\scripts\test-gateway-e2e.mjs`
Expected: `GET /api/analytics/reasoning?date_from=...&date_to=...` 仍只返回最近窗口，断言失败。

- [ ] **Step 3: 扩展 reasoning 观测接口**

```js
if (pathname === REASONING_BEHAVIOR_API_PATH && req.method === "GET") {
  const dateFrom = requestUrl.searchParams.get("date_from") || null;
  const dateTo = requestUrl.searchParams.get("date_to") || null;
  const samples = dateFrom || dateTo
    ? await readReasoningBehaviorSamplesByDateRange(runtime, dateFrom, dateTo)
    : runtime.monitor.reasoning_behavior_recent_samples || [];
  const snapshot = buildReasoningBehaviorSnapshotFromSamples(samples);
  jsonResponse(res, 200, { ok: true, date_from: dateFrom, date_to: dateTo, ...snapshot });
  return true;
}
```

- [ ] **Step 4: 重新跑测试，确认时间段观测可用**

Run: `node .\scripts\test-gateway-e2e.mjs`
Expected: 通过，且新的时间段查询断言通过。

### Task 2: 把观测大盘改成可选时间段

**Files:**
- Modify: `gateway.mjs`
- Test: `scripts/test-gateway-e2e.mjs`

- [ ] **Step 1: 先写 UI 时间段控件断言**

```js
assert(elements.reasoningDateFromInput, "reasoning 大盘缺少开始日期输入");
assert(elements.reasoningDateToInput, "reasoning 大盘缺少结束日期输入");
assert(elements.reasoningRangeApplyButton, "reasoning 大盘缺少范围应用按钮");
```

- [ ] **Step 2: 先让测试失败，确认 UI 还没有筛选控件**

Run: `node .\scripts\test-gateway-e2e.mjs`
Expected: 断言失败，提示缺少时间段控件。

- [ ] **Step 3: 给 reasoning 区块加日期筛选**

```html
<div class="range-bar">
  <input id="reasoningDateFromInput" type="date" />
  <input id="reasoningDateToInput" type="date" />
  <button id="reasoningRangeApplyButton" type="button">应用时间段</button>
</div>
```

```js
function getReasoningRangeParams() {
  return {
    date_from: refs.reasoningDateFromInput.value || null,
    date_to: refs.reasoningDateToInput.value || null,
  };
}
```

- [ ] **Step 4: 让大盘读取选中时间段的数据**

```js
const url = new URL(ui.reasoningBehaviorPath, window.location.origin);
const range = getReasoningRangeParams();
if (range.date_from) url.searchParams.set("date_from", range.date_from);
if (range.date_to) url.searchParams.set("date_to", range.date_to);
```

- [ ] **Step 5: 重新跑测试，确认 UI 能切换时间段**

Run: `node .\scripts\test-gateway-e2e.mjs`
Expected: 通过，且 UI 断言覆盖日期筛选控件与数据刷新。

### Task 3: 加强科技感展示但不改路由

**Files:**
- Modify: `gateway.mjs`

- [ ] **Step 1: 先补现有视觉结构检查**

```js
assert(html.includes("reasoning 行为统计"), "reasoning 大盘区块缺失");
assert(html.includes("range-bar"), "时间段筛选样式缺失");
```

- [ ] **Step 2: 先保留当前路由结构，只改管理页样式**

```css
.range-bar { display: flex; gap: 12px; padding: 12px; border: 1px solid rgba(255,255,255,.08); background: linear-gradient(135deg, rgba(15,23,42,.62), rgba(30,41,59,.42)); backdrop-filter: blur(16px); }
.neo-stat { box-shadow: 0 12px 40px rgba(34,197,94,.12); }
```

- [ ] **Step 3: 让科技感调整保持功能不变**

Run: `node --check .\gateway.mjs`
Expected: 语法检查通过，管理页样式更新不影响接口。

### Task 4: 回归与收口

**Files:**
- Modify: `scripts/test-gateway-e2e.mjs`
- Modify: `build.md`
- Modify: `err.md`

- [ ] **Step 1: 补时间段查询、UI 筛选、导出范围三类回归**
- [ ] **Step 2: 跑完整 E2E，确认状态接口、观测接口、导出接口都通过**
- [ ] **Step 3: 如出现新错误，沉淀到 `err.md`，避免重复踩坑**
- [ ] **Step 4: 更新 `build.md`，写清时间段观测大盘的验证命令**
