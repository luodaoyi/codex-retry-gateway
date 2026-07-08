package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestShouldStripEncryptedContentFromContinuationResponse(t *testing.T) {
	config := defaultGatewayConfig()

	if !shouldStripEncryptedContentFromContinuationResponse(config, "/responses", true, map[string]any{"stream": true}) {
		t.Fatal("expected streaming /responses continuation recovery request to strip encrypted content")
	}

	if shouldStripEncryptedContentFromContinuationResponse(config, "/responses", true, map[string]any{"stream": false}) {
		t.Fatal("did not expect non-streaming /responses request to strip encrypted content")
	}

	if shouldStripEncryptedContentFromContinuationResponse(config, "/responses", false, map[string]any{"stream": true}) {
		t.Fatal("did not expect non-inspected request to strip encrypted content")
	}

	config.StreamAction = "strict_502"
	if shouldStripEncryptedContentFromContinuationResponse(config, "/responses", true, map[string]any{"stream": true}) {
		t.Fatal("did not expect strict_502 mode to strip encrypted content on pass-through response")
	}
}

func TestDetectRequestKindWithStructuredMetadata(t *testing.T) {
	headers := map[string][]string{}
	requestJSON := map[string]any{
		"metadata": map[string]any{
			"nested": map[string]any{
				"request_kind": "context_compaction",
			},
		},
	}
	if got := detectRequestKind(headers, requestJSON); got != requestKindContextCompaction {
		t.Fatalf("expected %q, got %q", requestKindContextCompaction, got)
	}
}

func TestInspectSSEBodySupportsCRLF(t *testing.T) {
	body := []byte("event: response.output_item.added\r\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"abc\"}}\r\n\r\nevent: response.output_item.done\r\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\r\n\r\ndata: [DONE]\r\n\r\n")
	inspection := inspectSSEBody(defaultGatewayConfig(), body, &requestTracking{RequestKind: requestKindNormal})
	if len(inspection.Payloads) != 2 {
		t.Fatalf("expected 2 SSE payloads, got %d", len(inspection.Payloads))
	}
	if !inspection.Structure.HasReasoningItem || !inspection.Structure.HasFinalAnswer {
		t.Fatalf("expected CRLF SSE inspection to preserve reasoning and final answer structure, got %+v", inspection.Structure)
	}
}

func TestInspectSSEChunkAcrossChunkBoundary(t *testing.T) {
	state := &sseChunkState{}
	first := []byte("event: response.output_item.done\r\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",")
	second := []byte("\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}}\r\n\r\n")
	reasoning, payloads := inspectSSEChunk(state, first)
	if reasoning != nil || len(payloads) != 0 {
		t.Fatalf("expected no complete payloads from first partial chunk, got reasoning=%v payloads=%d", reasoning, len(payloads))
	}
	reasoning, payloads = inspectSSEChunk(state, second)
	if reasoning != nil {
		t.Fatalf("expected no reasoning tokens in completed payload, got %v", *reasoning)
	}
	if len(payloads) != 1 {
		t.Fatalf("expected 1 payload after completing chunk boundary, got %d", len(payloads))
	}
}

func TestRecordContinuationRecoverySuccessRequiresAttempt(t *testing.T) {
	mon := newMonitor()
	tracking := &requestTracking{}
	recordContinuationRecoverySuccess(mon, tracking)
	if mon.ContinuationRecoverySuccessCount != 0 {
		t.Fatalf("expected success count to stay 0 without attempt, got %d", mon.ContinuationRecoverySuccessCount)
	}
	recordContinuationRecoveryAttempt(mon, tracking)
	recordContinuationRecoverySuccess(mon, tracking)
	recordContinuationRecoverySuccess(mon, tracking)
	if mon.ContinuationRecoveryCount != 1 {
		t.Fatalf("expected attempt count 1, got %d", mon.ContinuationRecoveryCount)
	}
	if mon.ContinuationRecoverySuccessCount != 1 {
		t.Fatalf("expected success count 1 after duplicate success records, got %d", mon.ContinuationRecoverySuccessCount)
	}
}

func TestReasoningBehaviorCSVIncludesJSContractFields(t *testing.T) {
	text := buildReasoningBehaviorCSV([]reasoningSample{{
		SampleID:               "sample-1",
		GatewayRequestID:       "req-1",
		AttemptID:              "req-1:attempt:1",
		RequestPayloadExcerpt:  `{"redacted_sensitive_content":"x"}`,
		OutputTokens:           intPointer(12),
		TotalTokens:            intPointer(528),
		DurationTotalMS:        intPointer(1000),
		InternalRetryRemaining: intPointer(0),
	}})
	header := strings.Split(text, "\n")[0]
	for _, column := range []string{
		"attempt_id",
		"output_tokens",
		"total_tokens",
		"duration_total_ms",
		"output_tps",
		"reasoning_adjusted_tps",
		"upstream_stream_terminated",
		"internal_retry_attempt_index",
		"internal_retry_remaining",
		"upstream_http_status",
		"request_payload_excerpt",
	} {
		if !strings.Contains(header, column) {
			t.Fatalf("expected CSV header to include %q, got %s", column, header)
		}
	}
}

func TestBuildAnalysisSamplesPreviewUsesCollectedFields(t *testing.T) {
	samples := []reasoningSample{{
		SampleID:                   "sample-1",
		TS:                         "2026-07-08T00:00:00Z",
		RequestModel:               "gpt-5.5",
		RequestReasoningEffort:     "high",
		ReasoningTokens:            intPointer(516),
		OutputTokens:               intPointer(128),
		TotalTokens:                intPointer(700),
		DurationTotalMS:            intPointer(900),
		OutputTPS:                  142.2222,
		TimeNormalizationDeviation: 0.5,
		FinalAnswerOnly:            true,
		InternalRetryAttemptIndex:  1,
	}}
	preview := buildAnalysisSamplesPreview(samples)
	if len(preview) != 1 {
		t.Fatalf("expected one preview sample, got %d", len(preview))
	}
	if preview[0]["output_tokens"] != 128 || preview[0]["total_tokens"] != 700 || preview[0]["duration_total_ms"] != 900 {
		t.Fatalf("expected preview to include collected token and timing fields, got %#v", preview[0])
	}
	if preview[0]["internal_retry_attempt_index"] != 1 {
		t.Fatalf("expected preview retry index 1, got %#v", preview[0]["internal_retry_attempt_index"])
	}
}

func TestReasoningExportJobStatusIncludesMetadata(t *testing.T) {
	runtime := &appRuntime{
		Paths:             buildGatewayPaths(t.TempDir()),
		Monitor:           newMonitor(),
		ReasoningBehavior: newReasoningBehaviorState(),
	}
	job := &reasoningExportJob{
		JobID:     "job-1",
		Status:    "completed",
		Format:    "json",
		CreatedAt: "2026-07-08T00:00:00Z",
	}
	runtime.ReasoningBehavior.ExportJobs[job.JobID] = job
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("GET", reasoningBehaviorExportPath+"/jobs/job-1", nil)
	if !runtime.handleReasoningExportJob(recorder, request, reasoningBehaviorExportPath+"/jobs/job-1") {
		t.Fatal("expected export job status route to be handled")
	}
	body := recorder.Body.String()
	for _, field := range []string{"schema_version", "analytics_ready", "analytics_started_at", "analytics_state_root", "export_job"} {
		if !strings.Contains(body, field) {
			t.Fatalf("expected export job status to include %q, got %s", field, body)
		}
	}
}

func TestModelInsightsTracksFamilyMismatch(t *testing.T) {
	mon := newMonitor()
	context := createRequestModelContext("", "gpt-5.5")
	applyPayloadModelSignals(context, map[string]any{"model": "gpt-5.4"}, false, true)
	finalizeModelInsights(mon, "/responses", context, nil)
	snapshot := buildModelInsightsSnapshot(&appRuntime{Monitor: mon})
	consistency, _ := snapshot["consistency"].(map[string]any)
	if consistency["mismatched"] != 1 {
		t.Fatalf("expected one model mismatch, got %#v", consistency)
	}
	samples, _ := snapshot["suspicious_samples"].([]suspiciousModelSample)
	if len(samples) != 1 || samples[0].AnomalyType != "model_family_mismatch" {
		t.Fatalf("expected suspicious mismatch sample, got %#v", snapshot["suspicious_samples"])
	}
}

func TestManagementPayloadMatchesJSMessagesAndPaths(t *testing.T) {
	config := defaultGatewayConfig()
	runtime := &appRuntime{
		Config:            config,
		ConfigPath:        filepath.Join(t.TempDir(), "config.json"),
		LogPath:           filepath.Join(t.TempDir(), "gateway.log"),
		Paths:             buildGatewayPaths(t.TempDir()),
		Monitor:           newMonitor(),
		ReasoningBehavior: newReasoningBehaviorState(),
		HistoricalImports: newHistoricalImportState(),
		ActiveProbe:       newActiveProbeMonitor(),
		Logger:            func(string) {},
	}
	if err := os.MkdirAll(filepath.Dir(runtime.ConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}

	statusRecorder := httptest.NewRecorder()
	statusRequest := httptest.NewRequest("GET", statusAPIPath, nil)
	if !runtime.handleManagementRequest(statusRecorder, statusRequest, statusAPIPath) {
		t.Fatal("expected status route to be handled")
	}
	var statusPayload map[string]any
	if err := json.Unmarshal(statusRecorder.Body.Bytes(), &statusPayload); err != nil {
		t.Fatal(err)
	}
	paths, _ := statusPayload["paths"].(map[string]any)
	if _, ok := paths["analytics_root"]; ok {
		t.Fatalf("did not expect JS status paths to include analytics_root, got %#v", paths)
	}

	configRecorder := httptest.NewRecorder()
	configRequest := httptest.NewRequest("POST", configAPIPath, strings.NewReader(`{"log_match":true}`))
	if !runtime.handleConfigUpdate(configRecorder, configRequest) {
		t.Fatal("expected config update route to be handled")
	}
	if !strings.Contains(configRecorder.Body.String(), "配置已保存并立即生效") {
		t.Fatalf("expected JS config saved message, got %s", configRecorder.Body.String())
	}

	invalidProbeRecorder := httptest.NewRecorder()
	invalidProbeRequest := httptest.NewRequest("POST", probeRunAPIPath, strings.NewReader(`{`))
	if !runtime.handleManagementRequest(invalidProbeRecorder, invalidProbeRequest, probeRunAPIPath) {
		t.Fatal("expected probe route to be handled")
	}
	if invalidProbeRecorder.Code != 400 || !strings.Contains(invalidProbeRecorder.Body.String(), "主动探针请求必须是有效 JSON") {
		t.Fatalf("expected JS invalid probe message, code=%d body=%s", invalidProbeRecorder.Code, invalidProbeRecorder.Body.String())
	}

	reasoningAnalyzeRecorder := httptest.NewRecorder()
	reasoningAnalyzeRequest := httptest.NewRequest("POST", reasoningBehaviorAPIPath+"/analyze", strings.NewReader(`{`))
	if !runtime.handleManagementRequest(reasoningAnalyzeRecorder, reasoningAnalyzeRequest, reasoningBehaviorAPIPath+"/analyze") {
		t.Fatal("expected reasoning analyze route to be handled")
	}
	if reasoningAnalyzeRecorder.Code != 400 || !strings.Contains(reasoningAnalyzeRecorder.Body.String(), "reasoning 特征分析请求必须是有效 JSON。") {
		t.Fatalf("expected JS invalid reasoning analyze message, code=%d body=%s", reasoningAnalyzeRecorder.Code, reasoningAnalyzeRecorder.Body.String())
	}

	exportJobRecorder := httptest.NewRecorder()
	exportJobRequest := httptest.NewRequest("GET", reasoningBehaviorExportPath+"/jobs/missing", nil)
	if !runtime.handleReasoningExportJob(exportJobRecorder, exportJobRequest, reasoningBehaviorExportPath+"/jobs/missing") {
		t.Fatal("expected reasoning export job route to be handled")
	}
	if exportJobRecorder.Code != 404 || !strings.Contains(exportJobRecorder.Body.String(), "未找到 reasoning 导出任务。") {
		t.Fatalf("expected JS missing export job message, code=%d body=%s", exportJobRecorder.Code, exportJobRecorder.Body.String())
	}

	degraded := buildReasoningRangeDegradePayload(runtime, "2026-01-01", "2026-01-10", 7)
	if degraded["message"] != "时间段过大，已跳过明细读取；请缩小时间段或使用分片/压缩包导出。" {
		t.Fatalf("expected JS degraded message, got %#v", degraded["message"])
	}
	degradedSummary, _ := degraded["summary"].(map[string]any)
	if len(degradedSummary) != 2 || degradedSummary["wording"] != "统计结果用于发现候选复盘特征，不代表最终归因。" {
		t.Fatalf("expected JS degraded summary shape, got %#v", degradedSummary)
	}
}

func TestHistoricalImportRunJobAnalyzeAndLatestLifecycle(t *testing.T) {
	runtime := &appRuntime{
		Config:            defaultGatewayConfig(),
		Paths:             buildGatewayPaths(t.TempDir()),
		Monitor:           newMonitor(),
		ReasoningBehavior: newReasoningBehaviorState(),
		HistoricalImports: newHistoricalImportState(),
		Logger:            func(string) {},
	}
	runRecorder := httptest.NewRecorder()
	runRequest := httptest.NewRequest("POST", historicalImportAPIPath+"/run", strings.NewReader(`{"source_paths":{"cc_switch_sqlite":"Z:/missing.db"}}`))
	if !runtime.handleManagementRequest(runRecorder, runRequest, historicalImportAPIPath+"/run") {
		t.Fatal("expected historical import run route to be handled")
	}
	if runRecorder.Code != 202 {
		t.Fatalf("expected run status 202, got %d body=%s", runRecorder.Code, runRecorder.Body.String())
	}
	var runPayload map[string]any
	if err := json.Unmarshal(runRecorder.Body.Bytes(), &runPayload); err != nil {
		t.Fatal(err)
	}
	importJob, _ := runPayload["import_job"].(map[string]any)
	jobID, _ := importJob["job_id"].(string)
	if jobID == "" {
		t.Fatalf("expected job_id in run response, got %#v", runPayload)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		job := getHistoricalImportJob(runtime, jobID)
		if job != nil && (job.Status == "completed" || job.Status == "failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	jobRecorder := httptest.NewRecorder()
	jobRequest := httptest.NewRequest("GET", historicalImportAPIPath+"/jobs/"+jobID, nil)
	if !runtime.handleManagementRequest(jobRecorder, jobRequest, historicalImportAPIPath+"/jobs/"+jobID) {
		t.Fatal("expected historical import job route to be handled")
	}
	if jobRecorder.Code != 200 || !strings.Contains(jobRecorder.Body.String(), "feature_analysis") {
		t.Fatalf("expected completed job payload with feature_analysis, code=%d body=%s", jobRecorder.Code, jobRecorder.Body.String())
	}

	analyzeRecorder := httptest.NewRecorder()
	analyzeRequest := httptest.NewRequest("POST", historicalImportAPIPath+"/analyze", strings.NewReader(`{"job_id":"`+jobID+`"}`))
	if !runtime.handleManagementRequest(analyzeRecorder, analyzeRequest, historicalImportAPIPath+"/analyze") {
		t.Fatal("expected historical import analyze route to be handled")
	}
	if analyzeRecorder.Code != 200 || !strings.Contains(analyzeRecorder.Body.String(), "analysis_profile") {
		t.Fatalf("expected analyze response, code=%d body=%s", analyzeRecorder.Code, analyzeRecorder.Body.String())
	}

	latestRecorder := httptest.NewRecorder()
	latestRequest := httptest.NewRequest("GET", historicalImportAPIPath+"/latest", nil)
	if !runtime.handleManagementRequest(latestRecorder, latestRequest, historicalImportAPIPath+"/latest") {
		t.Fatal("expected historical import latest route to be handled")
	}
	if latestRecorder.Code != 200 || !strings.Contains(latestRecorder.Body.String(), jobID) {
		t.Fatalf("expected latest response to include job id, code=%d body=%s", latestRecorder.Code, latestRecorder.Body.String())
	}
}

func TestHistoricalImportQueuedJobPublicMatchesJSDefaults(t *testing.T) {
	job := &historicalImportJob{
		JobID:        "historical_import_test_1",
		Status:       "queued",
		CreatedAt:    "2026-07-08T00:00:00Z",
		Sources:      []historicalImportSource{{SourceType: "cc_switch_sqlite", Path: "missing.db", Status: "missing"}},
		TotalSources: 1,
		CurrentStep:  "queued",
	}
	payload := buildHistoricalImportJobPublic(job)
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	bodyText := string(body)
	for _, fragment := range []string{`"summary":null`, `"preflight":null`, `"feature_analysis":null`} {
		if !strings.Contains(bodyText, fragment) {
			t.Fatalf("expected JS null fragment %s in %s", fragment, bodyText)
		}
	}
	sources, _ := payload["sources"].([]historicalImportSource)
	if len(sources) != 0 {
		t.Fatalf("expected JS queued job public sources to default to empty result sources, got %#v", sources)
	}
}

func TestHistoricalImportSourcesMatchJSPayloadAndDefaultPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", "")

	defaultSources := buildHistoricalImportSources(map[string]any{})
	if len(defaultSources) != 4 {
		t.Fatalf("expected four JS default sources, got %#v", defaultSources)
	}
	expectedDefaults := []historicalImportSource{
		{SourceType: "cc_switch_sqlite", Path: filepath.Join(home, ".cc-switch", "cc-switch.db")},
		{SourceType: "codex_logs_sqlite", Path: filepath.Join(home, ".codex", "sqlite", "logs_2.sqlite")},
		{SourceType: "codex_logs_sqlite", Path: filepath.Join(home, ".codex", "logs_2.sqlite")},
		{SourceType: "codex_sessions_jsonl", Path: filepath.Join(home, ".codex", "sessions")},
	}
	for index, expected := range expectedDefaults {
		if defaultSources[index].SourceType != expected.SourceType || defaultSources[index].Path != expected.Path {
			t.Fatalf("default source %d mismatch: expected %#v got %#v", index, expected, defaultSources[index])
		}
	}

	customSources := buildHistoricalImportSources(map[string]any{"source_paths": map[string]any{
		"cc_switch_db":        filepath.Join(home, "cc.db"),
		"codex_logs_db":       filepath.Join(home, "logs-a.db"),
		"codex_logs_db_alt":   filepath.Join(home, "logs-b.db"),
		"codex_sessions_root": filepath.Join(home, "sessions"),
	}})
	if len(customSources) != 4 {
		t.Fatalf("expected JS payload keys to create four sources, got %#v", customSources)
	}
	if customSources[0].Path != filepath.Join(home, "cc.db") || customSources[2].Path != filepath.Join(home, "logs-b.db") {
		t.Fatalf("expected custom JS source path keys to be honored, got %#v", customSources)
	}
}

func TestHistoricalImportPreflightScansSessionFilesAndUsesJSResultShape(t *testing.T) {
	root := t.TempDir()
	sessionRoot := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Join(sessionRoot, "2026", "07"), 0o755); err != nil {
		t.Fatal(err)
	}
	smallFile := filepath.Join(sessionRoot, "small.jsonl")
	largeFile := filepath.Join(sessionRoot, "2026", "07", "large.jsonl")
	if err := os.WriteFile(smallFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(largeFile, []byte(strings.Repeat("x", 32)), 0o644); err != nil {
		t.Fatal(err)
	}

	result := buildHistoricalImportPreflightResult([]historicalImportSource{{
		SourceType: "codex_sessions_jsonl",
		Path:       sessionRoot,
		Status:     "pending",
	}})
	summary, _ := result["summary"].(map[string]any)
	if asFloat(summary["source_count"]) != 1 || asFloat(summary["session_file_count"]) != 2 {
		t.Fatalf("expected JS summary fields for session preflight, got %#v", summary)
	}
	sessions, _ := result["sessions"].(map[string]any)
	if asFloat(sessions["file_count"]) != 2 || asFloat(sessions["scanned_file_limit"]) != historicalImportSessionFileLimit {
		t.Fatalf("expected JS sessions shape, got %#v", sessions)
	}
	preflight, _ := result["preflight"].(map[string]any)
	if _, ok := preflight["can_build_reasoning_features"]; !ok {
		t.Fatalf("expected JS preflight decision fields, got %#v", preflight)
	}
}

func TestHistoricalImportSQLiteAnalysisDoesNotRequireExternalSQLite3(t *testing.T) {
	t.Setenv("PATH", "")
	root := t.TempDir()
	ccSwitchPath := filepath.Join(root, "cc-switch.db")
	ccDB, err := sql.Open("sqlite", ccSwitchPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ccDB.Close()
	_, err = ccDB.Exec(`
CREATE TABLE proxy_request_logs (
  status_code INTEGER,
  input_tokens INTEGER,
  output_tokens INTEGER,
  duration_ms INTEGER,
  latency_ms INTEGER,
  model TEXT,
  request_model TEXT,
  provider_id TEXT,
  provider_type TEXT,
  created_at TEXT
);
INSERT INTO proxy_request_logs(status_code,input_tokens,output_tokens,duration_ms,model,request_model,provider_id,created_at)
VALUES (200, 11, 7, 123, 'gpt-5.5', '', 'openai', '2026-07-08T00:00:00Z');
`)
	if err != nil {
		t.Fatal(err)
	}
	ccPart, err := analyzeCcSwitchDatabase(historicalImportSource{SourceType: "cc_switch_sqlite", Path: ccSwitchPath, Status: "pending"})
	if err != nil {
		t.Fatalf("expected pure Go SQLite analysis without sqlite3 in PATH, got %v", err)
	}
	ccSummary, _ := ccPart["summary"].(map[string]any)
	if asFloat(ccSummary["total_requests"]) != 1 || asFloat(ccSummary["successful_requests"]) != 1 {
		t.Fatalf("expected cc-switch summary from SQLite file, got %#v", ccSummary)
	}

	logsPath := filepath.Join(root, "logs_2.sqlite")
	logsDB, err := sql.Open("sqlite", logsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logsDB.Close()
	_, err = logsDB.Exec(`
CREATE TABLE logs (
  level TEXT,
  target TEXT,
  feedback_log_body TEXT
);
INSERT INTO logs(level,target,feedback_log_body)
VALUES ('info', 'codex', 'reasoning_tokens final_answer commentary 502');
`)
	if err != nil {
		t.Fatal(err)
	}
	logsPart, err := analyzeCodexLogsDatabase(historicalImportSource{SourceType: "codex_logs_sqlite", Path: logsPath, Status: "pending"})
	if err != nil {
		t.Fatalf("expected pure Go Codex logs SQLite analysis without sqlite3 in PATH, got %v", err)
	}
	logsSummary, _ := logsPart["summary"].(map[string]any)
	if asFloat(logsSummary["codex_log_rows"]) != 1 {
		t.Fatalf("expected codex log row count from SQLite file, got %#v", logsSummary)
	}
}

func TestActiveProbeRunLifecycleAndSnapshotShape(t *testing.T) {
	runtime := &appRuntime{
		Config:      defaultGatewayConfig(),
		Paths:       buildGatewayPaths(t.TempDir()),
		Monitor:     newMonitor(),
		ActiveProbe: newActiveProbeMonitor(),
		Logger:      func(string) {},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", probeRunAPIPath, strings.NewReader(`{"active_probe":{"target_families":["gpt-5.5"]}}`))
	if !runtime.handleManagementRequest(recorder, request, probeRunAPIPath) {
		t.Fatal("expected active probe run route to be handled")
	}
	if recorder.Code != 202 {
		t.Fatalf("expected active probe run status 202, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	snapshot := buildActiveProbeSnapshot(runtime)
	for _, field := range []string{
		"enabled",
		"running",
		"interval_ms",
		"target_families",
		"last_started_at",
		"total_runs",
		"skipped_runs",
		"indeterminate_count",
		"endpoint_success_counts",
		"probe_type_counts",
		"warning_type_counts",
		"violation_type_counts",
		"recent_samples",
	} {
		if _, ok := snapshot[field]; !ok {
			t.Fatalf("expected active probe snapshot field %q in %#v", field, snapshot)
		}
	}
	if snapshot["total_runs"] != 1 {
		t.Fatalf("expected total_runs=1, got %#v", snapshot["total_runs"])
	}
}

func TestActiveProbeRunExecutesDefaultUpstreamProbes(t *testing.T) {
	requestBodies := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		bodyText := string(body)
		requestBodies <- bodyText
		inputTokens := 130
		if strings.Contains(bodyText, "phase=baseline") {
			inputTokens = 100
		} else if strings.Contains(bodyText, "phase=seed") {
			inputTokens = 8292
		} else if strings.Contains(bodyText, "phase=budget") {
			inputTokens = 460000
		}
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(fmt.Sprintf(`{"id":"resp_probe","model":"gpt-5.5","usage":{"input_tokens":%d,"output_tokens":7,"total_tokens":130},"output_text":"OK"}`, inputTokens)))
	}))
	defer upstream.Close()

	config := defaultGatewayConfig()
	config.UpstreamBaseURL = upstream.URL
	config.ActiveProbe.TargetFamilies = []string{"gpt-5.5"}
	config.ActiveProbe.TimeoutMS = 2000
	runtime := &appRuntime{
		Config:      config,
		Paths:       buildGatewayPaths(t.TempDir()),
		Monitor:     newMonitor(),
		ActiveProbe: newActiveProbeMonitor(),
		Logger:      func(string) {},
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest("POST", probeRunAPIPath, strings.NewReader(`{"active_probe":{"target_families":["gpt-5.5"]}}`))
	if !runtime.handleManagementRequest(recorder, request, probeRunAPIPath) {
		t.Fatal("expected active probe run route to be handled")
	}
	if recorder.Code != 202 {
		t.Fatalf("expected active probe status 202, got %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"metrics"`) || !strings.Contains(recorder.Body.String(), `"reasoning_behavior"`) {
		t.Fatalf("expected probe run response to include dashboard payload, got %s", recorder.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := buildActiveProbeSnapshot(runtime)
		if snapshot["running"] == false {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot := buildActiveProbeSnapshot(runtime)
	if snapshot["running"] != false {
		t.Fatalf("expected probe run to finish, got %#v", snapshot)
	}
	probeTypeCounts, _ := snapshot["probe_type_counts"].(map[string]int)
	if probeTypeCounts["long_context"] != 1 || probeTypeCounts["image_input"] != 1 {
		t.Fatalf("expected default long_context and image_input probes to run, got %#v", probeTypeCounts)
	}
	if _, ok := probeTypeCounts["manual_lifecycle"]; ok {
		t.Fatalf("did not expect non-JS manual_lifecycle probe type, got %#v", probeTypeCounts)
	}
	if snapshot["pass_count"] != 2 {
		t.Fatalf("expected two passing probe samples, got %#v", snapshot)
	}
	seenImagePayload := false
	seenReasoningProfile := false
	for {
		select {
		case body := <-requestBodies:
			if strings.Contains(body, "input_image") {
				seenImagePayload = true
			}
			if strings.Contains(body, `"reasoning"`) && strings.Contains(body, `"effort":"medium"`) {
				seenReasoningProfile = true
			}
		default:
			goto drainedBodies
		}
	}
drainedBodies:
	if !seenImagePayload {
		t.Fatal("expected image_input probe to POST an input_image payload")
	}
	if !seenReasoningProfile {
		t.Fatal("expected active probe requests to include default reasoning effort profile")
	}
}

func TestActiveProbeOptionalProbeClassifiersRecordWarnings(t *testing.T) {
	var identityCount int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		bodyBytes, _ := io.ReadAll(request.Body)
		body := string(bodyBytes)
		writer.Header().Set("content-type", "application/json")
		switch {
		case strings.Contains(body, "__crg_response_structure_probe__"):
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output_text":"not json"}`))
		case strings.Contains(body, "__crg_identity_probe__"):
			count := atomic.AddInt32(&identityCount, 1)
			family := "gpt-5.4"
			if count%2 == 0 {
				family = "gpt-5.5"
			}
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output_text":"{\"self_reported_model\":\"gpt\",\"self_reported_family\":\"` + family + `\",\"claims_image_input\":true,\"claims_cutoff\":\"unknown\"}"}`))
		case strings.Contains(body, "__crg_knowledge_cutoff_probe__:self_cutoff"):
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output_text":"{\"claims_cutoff\":\"2024-01-01\"}"}`))
		case strings.Contains(body, "__crg_knowledge_cutoff_probe__"):
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output_text":"wrong"}`))
		default:
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output_text":"OK"}`))
		}
	}))
	defer upstream.Close()

	config := defaultGatewayConfig()
	config.UpstreamBaseURL = upstream.URL
	config.ActiveProbe.TargetFamilies = []string{"gpt-5.5"}
	config.ActiveProbe.TimeoutMS = 2000
	config.ActiveProbe.LongContext = map[string]any{"enabled": false}
	config.ActiveProbe.ImageInput = map[string]any{"enabled": false}
	config.ActiveProbe.ResponseStructure = map[string]any{"enabled": true, "repeat_count": 2}
	config.ActiveProbe.IdentityConsistency = map[string]any{"enabled": true, "repeat_count": 2}
	config.ActiveProbe.KnowledgeCutoff = map[string]any{"enabled": true, "max_questions": 3}
	runtime := &appRuntime{
		Config:      config,
		Paths:       buildGatewayPaths(t.TempDir()),
		Monitor:     newMonitor(),
		ActiveProbe: newActiveProbeMonitor(),
		Logger:      func(string) {},
	}

	response, statusCode := startActiveProbeRun(runtime, config.ActiveProbe, true)
	if statusCode != 202 || response["ok"] != true {
		t.Fatalf("expected active probe start success, status=%d response=%#v", statusCode, response)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := buildActiveProbeSnapshot(runtime)
		if snapshot["running"] == false {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot := buildActiveProbeSnapshot(runtime)
	probeTypeCounts, _ := snapshot["probe_type_counts"].(map[string]int)
	if probeTypeCounts["response_structure"] != 1 || probeTypeCounts["identity_consistency"] != 1 || probeTypeCounts["knowledge_cutoff"] != 1 {
		t.Fatalf("expected optional probe counts, got %#v", probeTypeCounts)
	}
	warningTypeCounts, _ := snapshot["warning_type_counts"].(map[string]int)
	for _, warningType := range []string{"probe_response_structure_warning", "probe_identity_consistency_warning", "probe_knowledge_cutoff_warning"} {
		if warningTypeCounts[warningType] != 1 {
			t.Fatalf("expected warning count for %s, got %#v", warningType, warningTypeCounts)
		}
	}
	if snapshot["warning_count"] != 3 {
		t.Fatalf("expected three warnings, got %#v", snapshot)
	}
}

func TestActiveProbeSkipsUntrackedLocalFamilyWithoutSelectedTargets(t *testing.T) {
	stateRoot := t.TempDir()
	configPath := filepath.Join(stateRoot, "config.toml")
	if err := os.WriteFile(configPath, []byte(`model = "claude-sonnet-4"`), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := buildGatewayPaths(stateRoot)
	if err := os.MkdirAll(filepath.Dir(paths.StatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	state := installState{CodexConfigPath: configPath}
	stateBody, _ := json.Marshal(state)
	if err := os.WriteFile(paths.StatePath, stateBody, 0o644); err != nil {
		t.Fatal(err)
	}
	config := defaultGatewayConfig()
	config.ActiveProbe.TargetFamilies = nil
	runtime := &appRuntime{
		Config:      config,
		Paths:       paths,
		Monitor:     newMonitor(),
		ActiveProbe: newActiveProbeMonitor(),
		Logger:      func(string) {},
	}

	response, statusCode := startActiveProbeRun(runtime, config.ActiveProbe, true)
	if statusCode != 202 || response["ok"] != true {
		t.Fatalf("expected skip response to be accepted, status=%d response=%#v", statusCode, response)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := buildActiveProbeSnapshot(runtime)
		if snapshot["running"] == false {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot := buildActiveProbeSnapshot(runtime)
	if snapshot["skipped_runs"] != 1 || snapshot["running"] != false {
		t.Fatalf("expected skipped run without active probe goroutine, got %#v", snapshot)
	}
	if snapshot["last_target_family"] != "other" {
		t.Fatalf("expected untracked local family to normalize to other, got %#v", snapshot["last_target_family"])
	}
}

func TestActiveProbeConfigUpdateSchedulesAndClearsTimers(t *testing.T) {
	config := defaultGatewayConfig()
	config.ActiveProbe.Enabled = false
	runtime := &appRuntime{
		Config:            config,
		ConfigPath:        filepath.Join(t.TempDir(), "config.json"),
		Paths:             buildGatewayPaths(t.TempDir()),
		Monitor:           newMonitor(),
		ReasoningBehavior: newReasoningBehaviorState(),
		ActiveProbe:       newActiveProbeMonitor(),
		Logger:            func(string) {},
	}
	if err := os.MkdirAll(filepath.Dir(runtime.ConfigPath), 0o755); err != nil {
		t.Fatal(err)
	}

	enableRecorder := httptest.NewRecorder()
	enableRequest := httptest.NewRequest("POST", configAPIPath, strings.NewReader(`{"active_probe":{"enabled":true,"target_families":["gpt-5.5"],"startup_delay_ms":60000,"interval_ms":900000}}`))
	if !runtime.handleConfigUpdate(enableRecorder, enableRequest) {
		t.Fatal("expected config update to be handled")
	}
	if enableRecorder.Code != 200 || runtime.ProbeStartupTimer == nil {
		t.Fatalf("expected active probe startup timer after enabling, code=%d body=%s", enableRecorder.Code, enableRecorder.Body.String())
	}
	if len(runtime.Config.ActiveProbe.TargetFamilies) != 1 || runtime.Config.ActiveProbe.TargetFamilies[0] != "gpt-5.5" {
		t.Fatalf("expected target family to stay model family, got %#v", runtime.Config.ActiveProbe.TargetFamilies)
	}

	disableRecorder := httptest.NewRecorder()
	disableRequest := httptest.NewRequest("POST", configAPIPath, strings.NewReader(`{"active_probe":{"enabled":false}}`))
	if !runtime.handleConfigUpdate(disableRecorder, disableRequest) {
		t.Fatal("expected config update to be handled")
	}
	if disableRecorder.Code != 200 || runtime.ProbeStartupTimer != nil || runtime.ProbeTimer != nil {
		t.Fatalf("expected active probe timers to be cleared after disabling, code=%d startup=%v timer=%v", disableRecorder.Code, runtime.ProbeStartupTimer, runtime.ProbeTimer)
	}
}

func TestActiveProbeAuthHeadersFollowProviderRequiresOpenAIAuth(t *testing.T) {
	stateRoot := t.TempDir()
	codexDir := filepath.Join(stateRoot, "codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatal(err)
	}
	codexConfigPath := filepath.Join(codexDir, "config.toml")
	codexConfig := `model_provider = "openai"

[model_providers.openai]
base_url = "https://api.openai.com"
requires_openai_auth = false
`
	if err := os.WriteFile(codexConfigPath, []byte(codexConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	paths := buildGatewayPaths(stateRoot)
	if err := os.MkdirAll(filepath.Dir(paths.StatePath), 0o755); err != nil {
		t.Fatal(err)
	}
	state := installState{CodexConfigPath: codexConfigPath, ProviderName: "openai", StateRoot: stateRoot}
	stateBody, _ := json.Marshal(state)
	if err := os.WriteFile(paths.StatePath, stateBody, 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := &appRuntime{Paths: paths}
	if got := buildActiveProbeHeaders(runtime).Get("authorization"); got != "" {
		t.Fatalf("did not expect auth when provider does not require OpenAI auth, got %q", got)
	}

	codexConfig = strings.Replace(codexConfig, "requires_openai_auth = false", "requires_openai_auth = true", 1)
	if err := os.WriteFile(codexConfigPath, []byte(codexConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"OPENAI_API_KEY":"config-dir-key"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "auth.json"), []byte(`{"OPENAI_API_KEY":"state-root-key"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := buildActiveProbeHeaders(runtime).Get("authorization"); got != "Bearer config-dir-key" {
		t.Fatalf("expected config-dir auth.json to win, got %q", got)
	}
}
