#!/usr/bin/env node

import http from "node:http";
import net from "node:net";
import { once } from "node:events";
import { mkdtemp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";

const scriptsRoot = import.meta.dirname;
const launchScript = path.join(scriptsRoot, "launch-ui.sh");
const restoreScript = path.join(scriptsRoot, "restore-codex-config.sh");

function assert(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}

function toUnixPathForBash(inputPath) {
  if (process.platform !== "win32") {
    return inputPath;
  }
  return `/mnt/${inputPath.slice(0, 1).toLowerCase()}${inputPath.slice(2).replace(/\\/g, "/")}`;
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
        "x-upstream-test": "unix-launch-ok",
      });
      res.end(JSON.stringify({ object: "list", data: [{ id: "unix-launch-model" }] }));
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
            id: "unix-launch-response",
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

async function runBashScript(scriptPath, args) {
  const bashScriptPath =
    process.platform === "win32"
      ? path.relative(process.cwd(), scriptPath).split(path.sep).join("/")
      : scriptPath;

  const bashArgs = [bashScriptPath, ...args];

  const child = spawn("bash", bashArgs, {
    stdio: ["ignore", "pipe", "pipe"],
  });

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
    throw new Error(`Bash script failed: ${scriptPath}\nstdout:\n${stdout}\nstderr:\n${stderr}`);
  }

  return { stdout, stderr };
}

async function run() {
  const tempRoot = await mkdtemp(path.join(os.tmpdir(), "codex-retry-gateway-unix-"));
  const codexDir = path.join(tempRoot, ".codex");
  const stateRoot = path.join(tempRoot, ".codex-retry-gateway");
  const codexConfigPath = path.join(codexDir, "config.toml");
  const upstreamPort = await getFreePort();
  const gatewayPort = await getFreePort();
  const gatewayBaseUrl = `http://127.0.0.1:${gatewayPort}`;
  const upstreamBaseUrl = `http://127.0.0.1:${upstreamPort}`;

  await mkdir(codexDir, { recursive: true });
  await writeFile(
    codexConfigPath,
    [
      'model_provider = "custom"',
      "",
      "[model_providers.custom]",
      'name = "Unix Launch Test"',
      `base_url = "${upstreamBaseUrl}"`,
      'wire_api = "responses"',
      "",
    ].join("\n"),
    "utf8",
  );

  const upstream = await startFakeUpstream(upstreamPort);

  try {
    await runBashScript(launchScript, [
      "--codex-config-path",
      toUnixPathForBash(codexConfigPath),
      "--state-root",
      toUnixPathForBash(stateRoot),
      "--listen-port",
      String(gatewayPort),
      "--no-open",
    ]);

    const installedConfig = await readFile(codexConfigPath, "utf8");
    assert(
      installedConfig.includes(`base_url = "${gatewayBaseUrl}"`),
      "Unix launch did not redirect the current provider to the local gateway",
    );
    const gatewayConfig = JSON.parse(
      await readFile(path.join(stateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      gatewayConfig.continuation_marker_text === "Continue thinking...",
      "Unix launch did not write default continuation_marker_text",
    );
    assert(
      gatewayConfig.reasoning_match_mode === "formula_518n_minus_2",
      "Unix launch did not write default reasoning_match_mode=formula_518n_minus_2",
    );
    assert(
      gatewayConfig.guard_retry_attempts === 5,
      "Unix launch did not write default guard_retry_attempts=5",
    );
    assert(
      gatewayConfig.stream_action === "continuation_recovery",
      "Unix launch did not write default stream_action=continuation_recovery",
    );
    await writeFile(
      path.join(stateRoot, "config", "config.json"),
      `${JSON.stringify(
        {
          ...gatewayConfig,
          intercept_rule_mode: "  Continuation_Recovery  ",
          continuation_marker_text: "  Unix custom marker  ",
          stream_action: undefined,
        },
        null,
        2,
      )}\n`,
      "utf8",
    );
    await runBashScript(launchScript, [
      "--codex-config-path",
      toUnixPathForBash(codexConfigPath),
      "--state-root",
      toUnixPathForBash(stateRoot),
      "--listen-port",
      String(gatewayPort),
      "--no-open",
    ]);
    const reusedGatewayConfig = JSON.parse(
      await readFile(path.join(stateRoot, "config", "config.json"), "utf8"),
    );
    assert(
      reusedGatewayConfig.intercept_rule_mode === "reasoning_tokens",
      "Unix launch reuse did not migrate legacy continuation_recovery intercept_rule_mode",
    );
    assert(
      reusedGatewayConfig.stream_action === "continuation_recovery",
      "Unix launch reuse did not migrate legacy continuation_recovery rule mode into stream_action",
    );
    assert(
      reusedGatewayConfig.continuation_marker_text === "  Unix custom marker  ",
      "Unix launch reuse did not preserve custom continuation_marker_text",
    );

    const uiResponse = await fetch(`${gatewayBaseUrl}/__codex_retry_gateway/ui`);
    assert(uiResponse.status === 200, `Unix UI page was not reachable: ${uiResponse.status}`);

    const proxiedModels = await fetch(`${gatewayBaseUrl}/v1/models`);
    assert(proxiedModels.status === 200, `/v1/models through unix launch flow failed: ${proxiedModels.status}`);
    assert(
      proxiedModels.headers.get("x-upstream-test") === "unix-launch-ok",
      "Unix launch gateway did not preserve upstream headers",
    );

    await runBashScript(restoreScript, [
      "--codex-config-path",
      toUnixPathForBash(codexConfigPath),
      "--state-root",
      toUnixPathForBash(stateRoot),
    ]);

    const restoredConfig = await readFile(codexConfigPath, "utf8");
    assert(
      restoredConfig.includes(`base_url = "${upstreamBaseUrl}"`),
      "Unix restore did not recover original base_url",
    );

    process.stdout.write("PASS unix launch-ui flow\n");
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
