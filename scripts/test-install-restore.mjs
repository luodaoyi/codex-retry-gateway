#!/usr/bin/env node

import http from "node:http";
import net from "node:net";
import { once } from "node:events";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";

const scriptsRoot = import.meta.dirname;
const installScript = path.join(scriptsRoot, "install-for-current-provider.ps1");
const launchUiScript = path.join(scriptsRoot, "launch-ui.ps1");
const restoreScript = path.join(scriptsRoot, "restore-codex-config.ps1");

function assert(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}

async function getFreePort() {
  const server = net.createServer();
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  const address = server.address();
  const port = address && typeof address === "object" ? address.port : null;
  server.close();
  await once(server, "close");
  if (!port) {
    throw new Error("Failed to allocate a free port");
  }
  return port;
}

function startFakeUpstream(port) {
  const server = http.createServer((req, res) => {
    if (req.method === "GET" && req.url === "/v1/models") {
      res.writeHead(200, {
        "content-type": "application/json; charset=utf-8",
        "x-upstream-test": "install-flow-ok",
      });
      res.end(JSON.stringify({ object: "list", data: [{ id: "install-test-model" }] }));
      return;
    }

    if (req.method === "POST" && (req.url === "/responses" || req.url === "/v1/responses")) {
      let body = "";
      req.setEncoding("utf8");
      req.on("data", (chunk) => {
        body += chunk;
      });
      req.on("end", () => {
        const parsed = JSON.parse(body || "{}");
        const reasoning = parsed.test_reasoning_tokens ?? 128;
        res.writeHead(200, { "content-type": "application/json; charset=utf-8" });
        res.end(
          JSON.stringify({
            id: "install-test-response",
            usage: {
              output_tokens_details: {
                reasoning_tokens: reasoning,
              },
            },
          }),
        );
      });
      return;
    }

    res.writeHead(404, { "content-type": "application/json; charset=utf-8" });
    res.end(JSON.stringify({ error: "not found" }));
  });

  return new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(port, "127.0.0.1", () => resolve(server));
  });
}

async function runPowerShellScript(scriptPath, args) {
  const child = spawn(
    "powershell",
    ["-ExecutionPolicy", "Bypass", "-File", scriptPath, ...args],
    { stdio: ["ignore", "pipe", "pipe"] },
  );

  let stdout = "";
  let stderr = "";
  child.stdout.on("data", (chunk) => {
    stdout += chunk.toString();
  });
  child.stderr.on("data", (chunk) => {
    stderr += chunk.toString();
  });

  const [exitCode] = await once(child, "exit");
  if (exitCode !== 0) {
    throw new Error(`PowerShell script failed: ${scriptPath}\nstdout:\n${stdout}\nstderr:\n${stderr}`);
  }

  return { stdout, stderr };
}

async function run() {
  const tempRoot = await mkdtemp(path.join(os.tmpdir(), "codex-retry-gateway-install-"));
  const codexDir = path.join(tempRoot, ".codex");
  const stateRoot = path.join(tempRoot, ".codex-retry-gateway");
  const legacyStateRoot = path.join(tempRoot, ".codex-retry-gateway-legacy");
  const legacyCodexDir = path.join(tempRoot, ".codex-legacy");
  const legacyCodexConfigPath = path.join(legacyCodexDir, "config.toml");
  const legacyGatewayPort = await getFreePort();
  const codexConfigPath = path.join(codexDir, "config.toml");
  const upstreamPort = await getFreePort();
  const gatewayPort = await getFreePort();

  await mkdir(codexDir, { recursive: true });
  await mkdir(legacyCodexDir, { recursive: true });
  await writeFile(
    codexConfigPath,
    [
      'model_provider = "custom"',
      "",
      "[model_providers.custom]",
      'name = "Install Test"',
      `base_url = "http://127.0.0.1:${upstreamPort}"`,
      'wire_api = "responses"',
      "",
    ].join("\n"),
    "utf8",
  );
  await writeFile(
    legacyCodexConfigPath,
    [
      'model_provider = "custom"',
      "",
      "[model_providers.custom]",
      'name = "Legacy Install Test"',
      `base_url = "http://127.0.0.1:${upstreamPort}"`,
      'wire_api = "responses"',
      "",
    ].join("\n"),
    "utf8",
  );

  const upstream = await startFakeUpstream(upstreamPort);

  try {
    await runPowerShellScript(installScript, [
      "-CodexConfigPath",
      codexConfigPath,
      "-StateRoot",
      stateRoot,
      "-ListenPort",
      String(gatewayPort),
    ]);

    const updatedConfig = await readFile(codexConfigPath, "utf8");
    assert(
      updatedConfig.includes(`base_url = "http://127.0.0.1:${gatewayPort}"`),
      "Install script did not redirect base_url to local gateway",
    );

    const gatewayConfig = JSON.parse(
      await readFile(path.join(stateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      gatewayConfig.upstream_base_url === `http://127.0.0.1:${upstreamPort}`,
      "Gateway config did not preserve original upstream_base_url",
    );
    assert(
      gatewayConfig.request_body_limit_bytes === 100 * 1024 * 1024,
      "Gateway config default request_body_limit_bytes should be 100MB",
    );
    assert(
      JSON.stringify(gatewayConfig.reasoning_equals) === JSON.stringify([516, 1034, 1552]),
      "Gateway config default reasoning_equals did not include 516,1034,1552",
    );
    assert(
      gatewayConfig.intercept_rule_mode === "reasoning_tokens",
      "Gateway config default intercept_rule_mode should be reasoning_tokens",
    );
    assert(
      gatewayConfig.reasoning_match_mode === "formula_518n_minus_2",
      "Gateway config default reasoning_match_mode should be formula_518n_minus_2",
    );
    assert(gatewayConfig.intercept_streaming === true, "Gateway config default intercept_streaming should be true");
    assert(
      gatewayConfig.intercept_non_streaming === true,
      "Gateway config default intercept_non_streaming should be true",
    );
    assert(gatewayConfig.guard_retry_attempts === 5, "Gateway config default guard_retry_attempts should be 5");
    assert(
      gatewayConfig.retry_upstream_capacity_errors === true,
      "Gateway config default retry_upstream_capacity_errors should be true",
    );
    assert(
      gatewayConfig.continuation_marker_text === "Continue thinking...",
      "Gateway config default continuation_marker_text should be present",
    );
    assert(
      gatewayConfig.stream_action === "continuation_recovery",
      "Gateway config default stream_action should be continuation_recovery",
    );
    await mkdir(path.join(legacyStateRoot, "config"), { recursive: true });
    const legacyGatewayConfig = {
      ...gatewayConfig,
      listen_port: legacyGatewayPort,
      request_body_limit_bytes: 10 * 1024 * 1024,
      intercept_rule_mode: "  Continuation_Recovery  ",
      continuation_marker_text: "  自定义续写 marker  ",
    };
    delete legacyGatewayConfig.intercept_streaming;
    delete legacyGatewayConfig.intercept_non_streaming;
    delete legacyGatewayConfig.reasoning_match_mode;
    delete legacyGatewayConfig.stream_action;
    delete legacyGatewayConfig.guard_retry_attempts;
    delete legacyGatewayConfig.retry_upstream_capacity_errors;
    await writeFile(
      path.join(legacyStateRoot, "config", "config.json"),
      `${JSON.stringify(legacyGatewayConfig, null, 2)}\n`,
      "utf8",
    );
    await runPowerShellScript(installScript, [
      "-CodexConfigPath",
      legacyCodexConfigPath,
      "-StateRoot",
      legacyStateRoot,
      "-ListenPort",
      String(legacyGatewayPort),
    ]);
    const reinstalledGatewayConfig = JSON.parse(
      await readFile(path.join(legacyStateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      reinstalledGatewayConfig.intercept_streaming === true,
      "Install script did not migrate missing intercept_streaming",
    );
    assert(
      reinstalledGatewayConfig.intercept_non_streaming === true,
      "Install script did not migrate missing intercept_non_streaming",
    );
    assert(
      reinstalledGatewayConfig.intercept_rule_mode === "reasoning_tokens",
      "Install script did not migrate legacy continuation_recovery intercept_rule_mode",
    );
    assert(
      reinstalledGatewayConfig.stream_action === "continuation_recovery",
      "Install script did not migrate legacy continuation_recovery rule mode into stream_action",
    );
    assert(
      reinstalledGatewayConfig.reasoning_match_mode === "formula_518n_minus_2",
      "Install script did not migrate missing reasoning_match_mode to formula_518n_minus_2",
    );
    assert(
      reinstalledGatewayConfig.guard_retry_attempts === 5,
      "Install script did not migrate missing guard_retry_attempts",
    );
    assert(
      reinstalledGatewayConfig.retry_upstream_capacity_errors === true,
      "Install script did not migrate missing retry_upstream_capacity_errors",
    );
    assert(
      reinstalledGatewayConfig.request_body_limit_bytes === 100 * 1024 * 1024,
      "Install script did not migrate legacy 10MB request_body_limit_bytes",
    );
    assert(
      reinstalledGatewayConfig.continuation_marker_text === "  自定义续写 marker  ",
      `Install script did not preserve continuation_marker_text: ${JSON.stringify(reinstalledGatewayConfig.continuation_marker_text)}`,
    );
    await runPowerShellScript(restoreScript, [
      "-CodexConfigPath",
      legacyCodexConfigPath,
      "-StateRoot",
      legacyStateRoot,
    ]);
    delete gatewayConfig.intercept_streaming;
    delete gatewayConfig.intercept_non_streaming;
    gatewayConfig.intercept_rule_mode = "  Continuation_Recovery  ";
    gatewayConfig.reasoning_match_mode = "formula_518n_minus_2";
    gatewayConfig.continuation_marker_text = "  Launch reuse marker  ";
    delete gatewayConfig.stream_action;
    delete gatewayConfig.guard_retry_attempts;
    delete gatewayConfig.retry_upstream_capacity_errors;
    gatewayConfig.request_body_limit_bytes = 10 * 1024 * 1024;
    await writeFile(
      path.join(stateRoot, "config", "config.json"),
      `${JSON.stringify(gatewayConfig, null, 2)}\n`,
      "utf8",
    );
    await runPowerShellScript(launchUiScript, [
      "-CodexConfigPath",
      codexConfigPath,
      "-StateRoot",
      stateRoot,
      "-ListenPort",
      String(gatewayPort),
      "-NoOpen",
    ]);
    const migratedGatewayConfig = JSON.parse(
      await readFile(path.join(stateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      migratedGatewayConfig.intercept_streaming === true,
      "Launch UI reuse did not migrate missing intercept_streaming",
    );
    assert(
      migratedGatewayConfig.intercept_non_streaming === true,
      "Launch UI reuse did not migrate missing intercept_non_streaming",
    );
    assert(
      migratedGatewayConfig.intercept_rule_mode === "reasoning_tokens",
      "Launch UI reuse did not migrate legacy continuation_recovery intercept_rule_mode",
    );
    assert(
      migratedGatewayConfig.stream_action === "continuation_recovery",
      "Launch UI reuse did not migrate legacy continuation_recovery rule mode into stream_action",
    );
    assert(
      migratedGatewayConfig.reasoning_match_mode === "formula_518n_minus_2",
      "Launch UI reuse did not preserve formula reasoning_match_mode",
    );
    assert(
      migratedGatewayConfig.continuation_marker_text === "  Launch reuse marker  ",
      `Launch UI reuse did not preserve continuation_marker_text: ${JSON.stringify(migratedGatewayConfig.continuation_marker_text)}`,
    );
    assert(
      migratedGatewayConfig.guard_retry_attempts === 5,
      "Launch UI reuse did not migrate missing guard_retry_attempts",
    );
    assert(
      migratedGatewayConfig.retry_upstream_capacity_errors === true,
      "Launch UI reuse did not migrate missing retry_upstream_capacity_errors",
    );
    assert(
      migratedGatewayConfig.request_body_limit_bytes === 100 * 1024 * 1024,
      "Launch UI reuse did not migrate legacy 10MB request_body_limit_bytes",
    );
    assert(Array.isArray(gatewayConfig.endpoints), "Gateway config endpoints must be an array");
    assert(
      gatewayConfig.endpoints.includes("/responses") &&
        gatewayConfig.endpoints.includes("/chat/completions") &&
        gatewayConfig.endpoints.includes("/v1/responses") &&
        gatewayConfig.endpoints.includes("/v1/chat/completions"),
      "Gateway config endpoints did not include both root and /v1 variants",
    );

    const proxiedModels = await fetch(`http://127.0.0.1:${gatewayPort}/v1/models`);
    assert(proxiedModels.status === 200, `/v1/models through installed gateway failed: ${proxiedModels.status}`);
    assert(
      proxiedModels.headers.get("x-upstream-test") === "install-flow-ok",
      "Installed gateway did not preserve upstream header",
    );

    const uiResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/ui`);
    const uiHtml = await uiResponse.text();
    assert(uiResponse.status === 200, `Management UI failed to load: ${uiResponse.status}`);
    assert(uiHtml.includes("Codex Retry Gateway"), "Management UI HTML did not include expected title");
    assert(
      uiHtml.includes("模型家族一致性（被动探针）"),
      "Management UI HTML did not include passive probe model consistency title",
    );
    assert(!uiHtml.includes("516 命中次数"), "Management UI HTML should not include removed 516 match stats");
    assert(!uiHtml.includes("516 占比"), "Management UI HTML should not include removed 516 ratio stats");
    assert(uiHtml.includes("当前规则命中总数"), "Management UI HTML did not include current rule match stats");
    assert(uiHtml.includes("实际拦截总数"), "Management UI HTML did not include actual block total stats");
    assert(uiHtml.includes("实际拦截占比"), "Management UI HTML did not include actual block ratio stats");
    assert(uiHtml.includes('id="guardRetryAttemptsInput"'), "Management UI HTML did not include guard retry input");
    assert(uiHtml.includes("当前生效策略"), "Management UI HTML did not include policy summary");
    assert(uiHtml.includes("命中后处理"), "Management UI HTML did not include post-hit action section");
    assert(uiHtml.includes("命中后最大内部尝试次数"), "Management UI HTML did not include guard retry label");
    assert(
      uiHtml.includes('id="retryUpstreamCapacityErrorsInput"'),
      "Management UI HTML did not include upstream capacity retry input",
    );
    assert(
      uiHtml.includes("上游 capacity 错误内重试"),
      "Management UI HTML did not include upstream capacity retry label",
    );
    assert(uiHtml.includes("TG群："), "Management UI HTML did not include Telegram group label");
    assert(uiHtml.includes('href="https://t.me/AI_INPUT_IM"'), "Management UI HTML did not include Telegram group link");
    assert(uiHtml.includes("实时日志"), "Management UI HTML did not include live log panel");
    assert(uiHtml.includes("主动探针"), "Management UI HTML did not include active probe panel");

    const statusResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/status`);
    const statusPayload = await statusResponse.json();
    assert(statusResponse.status === 200, `Status API failed: ${statusResponse.status}`);
    assert(statusPayload.config?.upstream_base_url === `http://127.0.0.1:${upstreamPort}`, "Status API did not expose config");
    assert(
      JSON.stringify(statusPayload.config?.reasoning_equals) === JSON.stringify([516, 1034, 1552]),
      "Status API did not expose default reasoning_equals",
    );
    assert(
      statusPayload.config?.intercept_rule_mode === "reasoning_tokens",
      "Status API did not expose intercept_rule_mode default",
    );
    assert(
      statusPayload.config?.reasoning_match_mode === "formula_518n_minus_2",
      "Status API did not expose reasoning_match_mode default",
    );
    assert(statusPayload.config?.intercept_streaming === true, "Status API did not expose intercept_streaming default");
    assert(
      statusPayload.config?.intercept_non_streaming === true,
      "Status API did not expose intercept_non_streaming default",
    );
    assert(
      statusPayload.config?.request_body_limit_bytes === 100 * 1024 * 1024,
      "Status API did not expose upgraded request_body_limit_bytes default",
    );
    assert(statusPayload.config?.guard_retry_attempts === 5, "Status API did not expose guard_retry_attempts default");
    assert(
      statusPayload.config?.retry_upstream_capacity_errors === true,
      "Status API did not expose retry_upstream_capacity_errors default",
    );
    assert(statusPayload.state?.original_base_url === `http://127.0.0.1:${upstreamPort}`, "Status API did not expose install state");
    assert(statusPayload.metrics?.inspected_response_count === 0, "Status API did not expose initial inspected count");
    assert(statusPayload.metrics?.reasoning_516_count === 0, "Status API did not expose initial 516 count");
    assert(statusPayload.active_probe, "Status API did not expose active_probe");
    assert(statusPayload.active_probe.enabled === false, "Initial active_probe.enabled should be false");
    assert(Array.isArray(statusPayload.active_probe.recent_samples), "Initial active_probe.recent_samples should be an array");

    const normalResponse = await fetch(`http://127.0.0.1:${gatewayPort}/responses`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ test_reasoning_tokens: 128 }),
    });
    assert(normalResponse.status === 200, `Expected a passthrough response before 516 test: ${normalResponse.status}`);

    const disableGuardRetryResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/config`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ guard_retry_attempts: 0 }),
    });
    assert(
      disableGuardRetryResponse.status === 200,
      `Disable guard retry API failed: ${disableGuardRetryResponse.status}`,
    );

    const blocked516Response = await fetch(`http://127.0.0.1:${gatewayPort}/responses`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ test_reasoning_tokens: 516 }),
    });
    assert(blocked516Response.status === 502, `Default 516 block did not trigger: ${blocked516Response.status}`);

    const metricsStatusResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/status`);
    const metricsStatusPayload = await metricsStatusResponse.json();
    assert(metricsStatusResponse.status === 200, `Status API failed after traffic: ${metricsStatusResponse.status}`);
    assert(metricsStatusPayload.metrics?.inspected_response_count === 2, "Status API inspected count was not updated");
    assert(metricsStatusPayload.metrics?.matched_response_count === 1, "Status API matched count was not updated");
    assert(
      metricsStatusPayload.metrics?.matched_non_streaming_count === 1,
      "Status API non-stream matched count was not updated",
    );
    assert(
      metricsStatusPayload.metrics?.blocked_non_streaming_count === 1,
      "Status API non-stream blocked count was not updated",
    );
    assert(metricsStatusPayload.metrics?.reasoning_516_count === 1, "Status API 516 count was not updated");
    assert(metricsStatusPayload.metrics?.reasoning_516_ratio === 0.5, "Status API 516 ratio was not updated");

    const logsResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/logs`);
    const logsPayload = await logsResponse.json();
    assert(logsResponse.status === 200, `Logs API failed: ${logsResponse.status}`);
    assert(Array.isArray(logsPayload.entries), "Logs API did not return entries array");
    assert(
      logsPayload.entries.some((entry) => `${entry.message || ""}`.includes("[start]")),
      "Logs API did not include gateway start log",
    );
    assert(
      logsPayload.entries.some((entry) => `${entry.message || ""}`.includes("reasoning_tokens=516")),
      "Logs API did not include 516 match log",
    );

    const saveConfigResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/config`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        reasoning_equals: [1024],
        endpoints: ["/responses", "/v1/responses"],
        intercept_rule_mode: "final_answer_only_high_xhigh",
        reasoning_match_mode: "manual",
        intercept_streaming: true,
        intercept_non_streaming: false,
        non_stream_status_code: 503,
        guard_retry_attempts: 2,
        retry_upstream_capacity_errors: false,
        stream_action: "continuation_recovery",
        continuation_marker_text: "  API marker  ",
        log_match: false,
        active_probe: {
          enabled: true,
          interval_ms: 11 * 60 * 1000,
          target_families: ["gpt-5.4"],
        },
      }),
    });
    const saveConfigPayload = await saveConfigResponse.json();
    assert(saveConfigResponse.status === 200, `Save config API failed: ${saveConfigResponse.status}`);
    assert(saveConfigPayload.config?.non_stream_status_code === 503, "Save config API did not return updated config");
    assert(saveConfigPayload.config?.guard_retry_attempts === 2, "Save config API did not return guard_retry_attempts");
    assert(
      saveConfigPayload.config?.retry_upstream_capacity_errors === false,
      "Save config API did not return retry_upstream_capacity_errors",
    );
    assert(
      saveConfigPayload.config?.intercept_rule_mode === "final_answer_only_high_xhigh",
      "Save config API did not return intercept_rule_mode",
    );
    assert(
      saveConfigPayload.config?.stream_action === "continuation_recovery",
      "Save config API did not return stream_action",
    );
    assert(
      saveConfigPayload.config?.reasoning_match_mode === "manual",
      "Save config API did not return reasoning_match_mode",
    );
    assert(
      saveConfigPayload.config?.continuation_marker_text === "  API marker  ",
      `Save config API did not preserve continuation_marker_text: ${JSON.stringify(saveConfigPayload.config?.continuation_marker_text)}`,
    );
    assert(saveConfigPayload.config?.intercept_streaming === true, "Save config API did not return intercept_streaming");
    assert(
      saveConfigPayload.config?.intercept_non_streaming === false,
      "Save config API did not return intercept_non_streaming",
    );

    const updatedGatewayConfig = JSON.parse(
      await readFile(path.join(stateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      JSON.stringify(updatedGatewayConfig.reasoning_equals) === JSON.stringify([1024]),
      "Saved config file did not persist reasoning_equals",
    );
    assert(updatedGatewayConfig.intercept_streaming === true, "Saved config file did not persist intercept_streaming");
    assert(
      updatedGatewayConfig.intercept_rule_mode === "final_answer_only_high_xhigh",
      "Saved config file did not persist intercept_rule_mode",
    );
    assert(
      updatedGatewayConfig.intercept_non_streaming === false,
      "Saved config file did not persist intercept_non_streaming",
    );
    assert(updatedGatewayConfig.guard_retry_attempts === 2, "Saved config file did not persist guard_retry_attempts");
    assert(
      updatedGatewayConfig.retry_upstream_capacity_errors === false,
      "Saved config file did not persist retry_upstream_capacity_errors",
    );
    assert(
      updatedGatewayConfig.stream_action === "continuation_recovery",
      "Saved config file did not persist stream_action",
    );
    assert(
      updatedGatewayConfig.reasoning_match_mode === "manual",
      "Saved config file did not persist reasoning_match_mode",
    );
    assert(
      updatedGatewayConfig.continuation_marker_text === "  API marker  ",
      `Saved config file did not preserve continuation_marker_text: ${JSON.stringify(updatedGatewayConfig.continuation_marker_text)}`,
    );
    const resetMarkerResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/config`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        continuation_marker_text: "   ",
      }),
    });
    const resetMarkerPayload = await resetMarkerResponse.json();
    assert(resetMarkerResponse.status === 200, `Reset continuation marker API failed: ${resetMarkerResponse.status}`);
    assert(
      resetMarkerPayload.config?.continuation_marker_text === "Continue thinking...",
      `Blank continuation_marker_text should reset to default: ${JSON.stringify(resetMarkerPayload.config?.continuation_marker_text)}`,
    );
    const resetMarkerGatewayConfig = JSON.parse(
      await readFile(path.join(stateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      resetMarkerGatewayConfig.continuation_marker_text === "Continue thinking...",
      `Blank continuation_marker_text should persist default: ${JSON.stringify(resetMarkerGatewayConfig.continuation_marker_text)}`,
    );
    assert(
      resetMarkerGatewayConfig.active_probe?.enabled === true,
      "Saved config file did not persist active_probe.enabled",
    );
    assert(
      resetMarkerGatewayConfig.active_probe?.interval_ms === 11 * 60 * 1000,
      "Saved config file did not persist active_probe.interval_ms",
    );
    assert(
      JSON.stringify(resetMarkerGatewayConfig.active_probe?.target_families) === JSON.stringify(["gpt-5.4"]),
      "Saved config file did not persist active_probe.target_families",
    );
    const invalidAutoProbeResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/config`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        active_probe: {
          enabled: true,
          target_families: [],
        },
      }),
    });
    const invalidAutoProbePayload = await invalidAutoProbeResponse.json();
    assert(
      invalidAutoProbeResponse.status === 400,
      `未选中模型时开启自动探测应失败: ${invalidAutoProbeResponse.status}`,
    );
    assert(
      `${invalidAutoProbePayload?.error?.message || ""}`.includes("至少选择一个"),
      "未选中模型时开启自动探测应返回目标模型校验错误",
    );

    const invalidInterceptResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/config`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({
        intercept_streaming: false,
        intercept_non_streaming: false,
      }),
    });
    const invalidInterceptPayload = await invalidInterceptResponse.json();
    assert(invalidInterceptResponse.status === 400, `双关拦截配置应失败: ${invalidInterceptResponse.status}`);
    assert(
      `${invalidInterceptPayload?.error?.message || ""}`.includes("流式与非流式至少选择一个"),
      "双关拦截配置应返回拦截目标校验错误",
    );

    const incrementalLogsResponse = await fetch(
      `http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/logs?since_seq=${logsPayload.latest_seq}`,
    );
    const incrementalLogsPayload = await incrementalLogsResponse.json();
    assert(incrementalLogsResponse.status === 200, `Incremental logs API failed: ${incrementalLogsResponse.status}`);
    assert(
      incrementalLogsPayload.entries.some((entry) => `${entry.message || ""}`.includes("[config] updated")),
      "Incremental logs API did not include config update log",
    );
    assert(
      incrementalLogsPayload.entries.some((entry) =>
        `${entry.message || ""}`.includes("[config] updated") &&
        `${entry.message || ""}`.includes("stream_action=continuation_recovery"),
      ),
      "Config update log did not include stream_action",
    );

    const blockedAfterSave = await fetch(`http://127.0.0.1:${gatewayPort}/responses`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ test_reasoning_tokens: 1024 }),
    });
    assert(blockedAfterSave.status === 200, `仅流式模式下非流式命中应透传: ${blockedAfterSave.status}`);

    const restoreViaUiResponse = await fetch(`http://127.0.0.1:${gatewayPort}/__codex_retry_gateway/api/restore`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({}),
    });
    const restoreViaUiPayload = await restoreViaUiResponse.json();
    assert(restoreViaUiResponse.status === 202, `Restore API failed: ${restoreViaUiResponse.status}`);
    assert(restoreViaUiPayload.ok === true, "Restore API did not acknowledge the restore request");

    const restoreStartedAt = Date.now();
    while (Date.now() - restoreStartedAt < 10000) {
      const restoredCandidate = await readFile(codexConfigPath, "utf8");
      if (restoredCandidate.includes(`base_url = "http://127.0.0.1:${upstreamPort}"`)) {
        break;
      }
      await new Promise((resolve) => setTimeout(resolve, 200));
    }

    const restoredConfig = await readFile(codexConfigPath, "utf8");
    assert(
      restoredConfig.includes(`base_url = "http://127.0.0.1:${upstreamPort}"`),
      "Restore script did not recover original base_url",
    );

    process.stdout.write("PASS install-restore flow\n");
  } finally {
    upstream.close();
    await once(upstream, "close");
    await rm(tempRoot, { recursive: true, force: true });
  }
}

run().catch((error) => {
  process.stderr.write(`${error?.stack || error}\n`);
  process.exit(1);
});
