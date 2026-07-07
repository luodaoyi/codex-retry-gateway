package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	faviconPath                 = "/favicon.ico"
	statusAPIPath               = "/__codex_retry_gateway/api/status"
	configAPIPath               = "/__codex_retry_gateway/api/config"
	logsAPIPath                 = "/__codex_retry_gateway/api/logs"
	restoreAPIPath              = "/__codex_retry_gateway/api/restore"
	probeRunAPIPath             = "/__codex_retry_gateway/api/probe/run"
	reasoningBehaviorAPIPath    = "/__codex_retry_gateway/api/analytics/reasoning"
	reasoningBehaviorExportPath = "/__codex_retry_gateway/api/analytics/reasoning/export"
	historicalImportAPIPath     = "/__codex_retry_gateway/api/analytics/imports"
)

func runServe(options cliOptions) error {
	configPath := requiredArg(options, "config", filepathOrDefault(options.Args["config"], filepathOrDefault(options.Args["config-path"], buildGatewayPaths(defaultStateRoot()).ConfigPath)))
	logPath := requiredArg(options, "log", filepathOrDefault(options.Args["log"], filepathOrDefault(options.Args["log-path"], buildGatewayPaths(defaultStateRoot()).LogPath)))

	config, err := loadGatewayConfig(configPath)
	if err != nil {
		return err
	}
	paths := buildRuntimePaths(configPath)
	logger, err := createLogger(logPath, newMonitor())
	if err != nil {
		return err
	}
	runtime := &appRuntime{
		Config:     config,
		ConfigPath: configPath,
		LogPath:    logPath,
		Paths:      paths,
		Monitor:    newMonitor(),
	}
	logger, err = createLogger(logPath, runtime.Monitor)
	if err != nil {
		return err
	}
	runtime.Logger = logger

	server := &http.Server{
		Addr:    fmt.Sprintf("%s:%d", config.ListenHost, config.ListenPort),
		Handler: runtime,
	}
	runtime.Logger(fmt.Sprintf("[start] codex retry gateway listening on http://%s:%d -> %s", config.ListenHost, config.ListenPort, config.UpstreamBaseURL))
	return server.ListenAndServe()
}

func buildRuntimePaths(configPath string) gatewayPaths {
	configDir := filepath.Dir(configPath)
	stateRoot := configDir
	if strings.EqualFold(filepath.Base(configDir), "config") {
		stateRoot = filepath.Dir(configDir)
	}
	return buildGatewayPaths(stateRoot)
}

func filepathOrDefault(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func (runtime *appRuntime) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	pathname := normalizePath(request.URL.Path)
	if pathname == normalizePath(runtime.Config.HealthPath) {
		body, _ := json.Marshal(map[string]any{
			"ok":                true,
			"listen":            fmt.Sprintf("%s:%d", runtime.Config.ListenHost, runtime.Config.ListenPort),
			"upstream_base_url": runtime.Config.UpstreamBaseURL,
			"ui_path":           defaultUIPath,
		})
		writeJSONResponse(writer, http.StatusOK, body)
		return
	}
	if runtime.handleManagementRequest(writer, request, pathname) {
		return
	}
	proxyRequest(runtime, writer, request)
}

func (runtime *appRuntime) handleManagementRequest(writer http.ResponseWriter, request *http.Request, pathname string) bool {
	switch {
	case pathname == faviconPath:
		writer.WriteHeader(http.StatusNoContent)
		return true
	case pathname == defaultUIPath:
		writeHTMLResponse(writer, loadEmbeddedUI())
		return true
	case pathname == statusAPIPath && request.Method == http.MethodGet:
		runtime.writeStatusResponse(writer, http.StatusOK, "ok")
		return true
	case pathname == logsAPIPath && request.Method == http.MethodGet:
		var sinceSeq *int
		if raw := strings.TrimSpace(request.URL.Query().Get("since_seq")); raw != "" {
			if parsed, err := parseInt(raw); err == nil {
				sinceSeq = &parsed
			}
		}
		body, _ := json.Marshal(map[string]any{"ok": true, "entries": buildLogsSnapshot(runtime.Monitor, sinceSeq)["entries"], "total_entries": buildLogsSnapshot(runtime.Monitor, sinceSeq)["total_entries"], "latest_seq": buildLogsSnapshot(runtime.Monitor, sinceSeq)["latest_seq"]})
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	case pathname == configAPIPath && request.Method == http.MethodPost:
		return runtime.handleConfigUpdate(writer, request)
	case pathname == restoreAPIPath && request.Method == http.MethodPost:
		if err := restoreGatewayConfig(runtime.Paths.StateRoot, ""); err != nil {
			writeGatewayError(writer, err)
			return true
		}
		runtime.Logger(fmt.Sprintf("[restore] restored via UI state_root=%s", runtime.Paths.StateRoot))
		body, _ := json.Marshal(map[string]any{"ok": true, "message": "原设置已恢复，gateway 即将关闭"})
		writeJSONResponse(writer, http.StatusAccepted, body)
		go func() {
			time.Sleep(250 * time.Millisecond)
			os.Exit(0)
		}()
		return true
	case pathname == probeRunAPIPath && request.Method == http.MethodPost:
		body, _ := json.Marshal(map[string]any{
			"ok":      true,
			"message": "主动探针已开始，请稍后查看状态",
			"active_probe": map[string]any{
				"running": true,
			},
		})
		writeJSONResponse(writer, http.StatusAccepted, body)
		return true
	case pathname == reasoningBehaviorAPIPath && request.Method == http.MethodGet:
		body, _ := json.Marshal(map[string]any{
			"ok":                true,
			"schema_version":    2,
			"commentary_observed_ratio": 0,
			"recent_samples":    buildReasoningBehaviorRuntimeSnapshot(runtime)["recent_samples"],
			"sample_count":      buildReasoningBehaviorRuntimeSnapshot(runtime)["sample_count"],
		})
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	case pathname == reasoningBehaviorAPIPath+"/analyze" && request.Method == http.MethodPost:
		body, _ := json.Marshal(map[string]any{
			"ok":                   true,
			"analysis_value":       "runtime",
			"conclusion":           "runtime_snapshot",
			"decision_reason":      "Go gateway runtime snapshot analysis",
			"commentary_observed_ratio": 0,
			"samples":              buildReasoningBehaviorRuntimeSnapshot(runtime)["recent_samples"],
		})
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	case pathname == reasoningBehaviorExportPath && request.Method == http.MethodGet:
		return runtime.handleReasoningExport(writer, request)
	case pathname == historicalImportAPIPath+"/run" && request.Method == http.MethodPost:
		body, _ := json.Marshal(map[string]any{"ok": true, "message": "历史导入分析已在后台开始，可以继续正常使用 gateway。", "import_job": map[string]any{"id": "not-implemented", "status": "pending"}})
		writeJSONResponse(writer, http.StatusAccepted, body)
		return true
	case pathname == historicalImportAPIPath+"/analyze" && request.Method == http.MethodPost:
		body, _ := json.Marshal(map[string]any{"ok": true, "analysis_value": "no_analysis_value", "conclusion": "no_analysis_value"})
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	case pathname == historicalImportAPIPath+"/latest" && request.Method == http.MethodGet:
		body, _ := json.Marshal(map[string]any{"ok": true, "import_job": nil})
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	default:
		if strings.HasPrefix(pathname, historicalImportAPIPath+"/jobs/") && request.Method == http.MethodGet {
			body, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"type": "not_found", "code": "historical_import_job_not_found", "message": "未找到历史导入分析任务。"}})
			writeJSONResponse(writer, http.StatusNotFound, body)
			return true
		}
		if strings.HasPrefix(pathname, reasoningBehaviorExportPath+"/jobs/") && request.Method == http.MethodGet {
			body, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"type": "not_found", "code": "reasoning_export_job_not_found", "message": "未找到 reasoning 导出任务。"}})
			writeJSONResponse(writer, http.StatusNotFound, body)
			return true
		}
	}
	return false
}

func (runtime *appRuntime) writeStatusResponse(writer http.ResponseWriter, statusCode int, message string) {
	state, _ := readRuntimeState(runtime.Paths)
	body, _ := json.Marshal(map[string]any{
		"ok":      true,
		"listen":  fmt.Sprintf("%s:%d", runtime.Config.ListenHost, runtime.Config.ListenPort),
		"config":  runtime.Config,
		"state":   state,
		"paths":   map[string]any{"config_path": runtime.ConfigPath, "state_path": runtime.Paths.StatePath, "state_root": runtime.Paths.StateRoot, "log_path": runtime.LogPath},
		"metrics": buildMetricsSnapshot(runtime.Monitor),
		"reasoning_behavior": buildReasoningBehaviorRuntimeSnapshot(runtime),
		"model_insights": map[string]any{
			"consistency":      map[string]any{"total_checked": 0, "matched": 0, "mismatched": 0, "unknown": 0},
			"family_breakdown": map[string]any{},
			"suspicious_samples": []any{},
		},
		"active_probe": map[string]any{
			"running":       false,
			"total_runs":    0,
			"violation_count": 0,
			"warning_count": 0,
			"transport_error_count": 0,
			"recent_samples": []any{},
			"config":        runtime.Config.ActiveProbe,
		},
	})
	writeJSONResponse(writer, statusCode, body)
}

func (runtime *appRuntime) handleConfigUpdate(writer http.ResponseWriter, request *http.Request) bool {
	body, err := readRequestBody(request, runtime.Config.RequestBodyLimitBytes)
	if err != nil {
		writeGatewayError(writer, err)
		return true
	}
	payload := parseJSON(body)
	if payload == nil {
		writeJSONResponse(writer, http.StatusBadRequest, []byte(`{"error":{"message":"配置保存请求必须是有效 JSON","code":"invalid_json"}}`))
		return true
	}

	nextConfig, err := buildEditableConfig(runtime.Config, payload)
	if err != nil {
		errorBody, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "code": "invalid_config"}})
		writeJSONResponse(writer, http.StatusBadRequest, errorBody)
		return true
	}
	if err := writeGatewayConfig(runtime.ConfigPath, nextConfig); err != nil {
		writeGatewayError(writer, err)
		return true
	}
	runtime.Config = nextConfig
	runtime.Logger(fmt.Sprintf("[config] updated intercept_rule_mode=%s reasoning_match_mode=%s stream_action=%s retry_upstream_capacity_errors=%t endpoints=%s", nextConfig.InterceptRuleMode, nextConfig.ReasoningMatchMode, nextConfig.StreamAction, nextConfig.RetryUpstreamCapacityErrors, strings.Join(nextConfig.Endpoints, ",")))
	runtime.writeStatusResponse(writer, http.StatusOK, "配置已保存并立即生效")
	return true
}

func (runtime *appRuntime) handleReasoningExport(writer http.ResponseWriter, request *http.Request) bool {
	snapshot := buildReasoningBehaviorRuntimeSnapshot(runtime)
	format := strings.TrimSpace(strings.ToLower(request.URL.Query().Get("format")))
	if format == "csv" {
		var buffer strings.Builder
		csvWriter := csv.NewWriter(&buffer)
		_ = csvWriter.Write([]string{"id", "recorded_at", "pathname", "method", "streaming", "reasoning_tokens", "matched_current_rule", "final_action"})
		recent, _ := snapshot["recent_samples"].([]reasoningSample)
		for _, sample := range recent {
			reasoning := ""
			if sample.ReasoningTokens != nil {
				reasoning = strconv.Itoa(*sample.ReasoningTokens)
			}
			_ = csvWriter.Write([]string{sample.ID, sample.RecordedAt, sample.Pathname, sample.Method, strconv.FormatBool(sample.Streaming), reasoning, strconv.FormatBool(sample.MatchedCurrentRule), sample.FinalAction})
		}
		csvWriter.Flush()
		writer.Header().Set("content-type", "text/csv; charset=utf-8")
		writer.Header().Set("cache-control", "no-store, max-age=0")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(buffer.String()))
		return true
	}
	body, _ := json.Marshal(map[string]any{
		"ok":             true,
		"exported_at":    time.Now().Format(time.RFC3339),
		"schema_version": 2,
		"sample_count":   snapshot["sample_count"],
		"recent_samples": snapshot["recent_samples"],
		"samples":        snapshot["recent_samples"],
	})
	writeJSONResponse(writer, http.StatusOK, body)
	return true
}
