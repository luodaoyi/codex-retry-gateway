package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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
		Config:            config,
		ConfigPath:        configPath,
		LogPath:           logPath,
		Paths:             paths,
		Monitor:           newMonitor(),
		ReasoningBehavior: newReasoningBehaviorState(),
		HistoricalImports: newHistoricalImportState(),
		ActiveProbe:       newActiveProbeMonitor(),
	}
	logger, err = createLogger(logPath, runtime.Monitor)
	if err != nil {
		return err
	}
	runtime.Logger = logger
	scheduleActiveProbes(runtime)

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
		snapshot := buildLogsSnapshot(runtime.Monitor, sinceSeq)
		body, _ := json.Marshal(map[string]any{
			"ok":            true,
			"entries":       snapshot["entries"],
			"total_entries": snapshot["total_entries"],
			"latest_seq":    snapshot["latest_seq"],
		})
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
		requestBody, err := readRequestBody(request, runtime.Config.RequestBodyLimitBytes)
		if err != nil {
			writeGatewayError(writer, err)
			return true
		}
		payload := map[string]any{}
		if len(requestBody) > 0 {
			payload = parseJSON(requestBody)
			if payload == nil {
				writeJSONResponse(writer, http.StatusBadRequest, []byte(`{"error":{"message":"主动探针请求必须是有效 JSON","code":"invalid_json"}}`))
				return true
			}
		}
		nextActiveProbe := runtime.Config.ActiveProbe
		if raw, ok := payload["active_probe"]; ok {
			nextActiveProbe = normalizeActiveProbeConfig(raw, runtime.Config.ActiveProbe)
		}
		response, statusCode := startActiveProbeRun(runtime, nextActiveProbe, true)
		bodyPayload := runtime.buildStatusPayload()
		for key, value := range response {
			bodyPayload[key] = value
		}
		body, _ := json.Marshal(bodyPayload)
		writeJSONResponse(writer, statusCode, body)
		return true
	case pathname == reasoningBehaviorAPIPath && request.Method == http.MethodGet:
		return runtime.handleReasoningBehavior(writer, request)
	case pathname == reasoningBehaviorAPIPath+"/analyze" && request.Method == http.MethodPost:
		return runtime.handleReasoningAnalyze(writer, request)
	case pathname == reasoningBehaviorExportPath && request.Method == http.MethodGet:
		return runtime.handleReasoningExport(writer, request)
	case pathname == historicalImportAPIPath+"/run" && request.Method == http.MethodPost:
		requestBody, err := readRequestBody(request, runtime.Config.RequestBodyLimitBytes)
		if err != nil {
			writeGatewayError(writer, err)
			return true
		}
		payload := map[string]any{}
		if len(requestBody) > 0 {
			payload = parseJSON(requestBody)
			if payload == nil {
				writeJSONResponse(writer, http.StatusBadRequest, []byte(`{"ok":false,"error":{"type":"invalid_request","code":"invalid_json","message":"历史导入分析请求必须是有效 JSON。"}}`))
				return true
			}
		}
		job := startHistoricalImportJob(runtime, payload)
		body, _ := json.Marshal(map[string]any{"ok": true, "message": "历史导入分析已在后台开始，可以继续正常使用 gateway。", "import_job": buildHistoricalImportJobPublic(job)})
		writeJSONResponse(writer, http.StatusAccepted, body)
		return true
	case pathname == historicalImportAPIPath+"/analyze" && request.Method == http.MethodPost:
		requestBody, err := readRequestBody(request, runtime.Config.RequestBodyLimitBytes)
		if err != nil {
			writeGatewayError(writer, err)
			return true
		}
		payload := map[string]any{}
		if len(requestBody) > 0 {
			payload = parseJSON(requestBody)
			if payload == nil {
				writeJSONResponse(writer, http.StatusBadRequest, []byte(`{"ok":false,"error":{"type":"invalid_request","code":"invalid_json","message":"历史导入特征分析请求必须是有效 JSON。"}}`))
				return true
			}
		}
		requestedJobID := strings.TrimSpace(anyToString(payload["job_id"]))
		var job *historicalImportJob
		if requestedJobID != "" {
			job = getHistoricalImportJob(runtime, requestedJobID)
		} else {
			runtime.ReasoningMu.Lock()
			for _, candidate := range runtime.HistoricalImports.Jobs {
				if job == nil || candidate.CreatedAt > job.CreatedAt {
					job = candidate
				}
			}
			runtime.ReasoningMu.Unlock()
		}
		if job == nil {
			body, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"type": "not_found", "code": "historical_import_job_not_found", "message": "未找到可分析的历史导入任务。"}})
			writeJSONResponse(writer, http.StatusNotFound, body)
			return true
		}
		analysis := buildHistoricalFeatureAnalysisFromJob(job, payload)
		analysis["ok"] = true
		body, _ := json.Marshal(analysis)
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	case pathname == historicalImportAPIPath+"/latest" && request.Method == http.MethodGet:
		body, _ := json.Marshal(map[string]any{"ok": true, "import_job": latestHistoricalImportPublic(runtime)})
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	default:
		if strings.HasPrefix(pathname, historicalImportAPIPath+"/jobs/") && request.Method == http.MethodGet {
			jobID := strings.Trim(strings.TrimPrefix(pathname, historicalImportAPIPath+"/jobs/"), "/")
			job := getHistoricalImportJob(runtime, jobID)
			if job == nil {
				body, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"type": "not_found", "code": "historical_import_job_not_found", "message": "未找到历史导入分析任务。"}})
				writeJSONResponse(writer, http.StatusNotFound, body)
				return true
			}
			body, _ := json.Marshal(map[string]any{"ok": true, "import_job": buildHistoricalImportJobPublic(job)})
			writeJSONResponse(writer, http.StatusOK, body)
			return true
		}
		if strings.HasPrefix(pathname, reasoningBehaviorExportPath+"/jobs/") && request.Method == http.MethodGet {
			return runtime.handleReasoningExportJob(writer, request, pathname)
		}
	}
	return false
}

func (runtime *appRuntime) handleReasoningBehavior(writer http.ResponseWriter, request *http.Request) bool {
	dateFrom := normalizeDateKeyInput(request.URL.Query().Get("date_from"))
	dateTo := normalizeDateKeyInput(request.URL.Query().Get("date_to"))
	if rangeDays, ok := countInclusiveDateRangeDays(dateFrom, dateTo); ok && rangeDays > reasoningBehaviorMaxInlineRangeDays {
		body, _ := json.Marshal(buildReasoningRangeDegradePayload(runtime, dateFrom, dateTo, reasoningBehaviorMaxInlineRangeDays))
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	}

	responseBody := map[string]any{
		"ok":        true,
		"date_from": nullIfEmpty(dateFrom),
		"date_to":   nullIfEmpty(dateTo),
	}
	if dateFrom != "" || dateTo != "" {
		samples, err := readReasoningBehaviorSamplesByDateRange(runtime, dateFrom, dateTo)
		if err != nil {
			writeGatewayError(writer, err)
			return true
		}
		for key, value := range buildReasoningBehaviorMetadata(runtime) {
			responseBody[key] = value
		}
		for key, value := range buildReasoningBehaviorSnapshotFromSamples(samples, 50) {
			responseBody[key] = value
		}
	} else {
		for key, value := range buildReasoningBehaviorRuntimeSnapshot(runtime) {
			responseBody[key] = value
		}
	}
	body, _ := json.Marshal(responseBody)
	writeJSONResponse(writer, http.StatusOK, body)
	return true
}

func (runtime *appRuntime) handleReasoningAnalyze(writer http.ResponseWriter, request *http.Request) bool {
	requestBody, err := readRequestBody(request, runtime.Config.RequestBodyLimitBytes)
	if err != nil {
		writeGatewayError(writer, err)
		return true
	}
	payload := map[string]any{}
	if len(requestBody) > 0 {
		payload = parseJSON(requestBody)
		if payload == nil {
			writeJSONResponse(writer, http.StatusBadRequest, []byte(`{"ok":false,"error":{"type":"invalid_request","code":"invalid_json","message":"reasoning 特征分析请求必须是有效 JSON。"}}`))
			return true
		}
	}

	profile := buildReasoningAnalysisProfile(payload, "runtime")
	if rangeDays, ok := countInclusiveDateRangeDays(profile.Filters.DateFrom, profile.Filters.DateTo); ok && rangeDays > reasoningBehaviorMaxInlineRangeDays {
		analysis := buildFeatureAnalysisFromSamples([]reasoningSample{}, profile)
		analysis["analysis_value"] = "partial"
		analysis["conclusion"] = "insufficient_fields"
		analysis["decision_reason"] = "分析时间段过大，已跳过明细读取；请缩小时间段后再运行特征分析。"
		body, _ := json.Marshal(analysis)
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	}

	var samples []reasoningSample
	if profile.Filters.DateFrom != "" || profile.Filters.DateTo != "" {
		samples, err = readReasoningBehaviorSamplesByDateRange(runtime, profile.Filters.DateFrom, profile.Filters.DateTo)
		if err != nil {
			writeGatewayError(writer, err)
			return true
		}
	} else {
		runtime.ReasoningMu.Lock()
		samples = append([]reasoningSample{}, runtime.ReasoningBehavior.RecentSamples...)
		runtime.ReasoningMu.Unlock()
	}
	body, _ := json.Marshal(buildFeatureAnalysisFromSamples(samples, profile))
	writeJSONResponse(writer, http.StatusOK, body)
	return true
}

func (runtime *appRuntime) handleReasoningExportJob(writer http.ResponseWriter, request *http.Request, pathname string) bool {
	jobPath := strings.TrimPrefix(pathname, reasoningBehaviorExportPath+"/jobs/")
	jobParts := strings.Split(strings.Trim(jobPath, "/"), "/")
	if len(jobParts) == 0 || jobParts[0] == "" {
		return false
	}
	jobID := jobParts[0]
	job := getReasoningExportJob(runtime, jobID)
	if len(jobParts) == 1 {
		if job == nil {
			body, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"type": "not_found", "code": "reasoning_export_job_not_found", "message": "未找到 reasoning 导出任务。"}})
			writeJSONResponse(writer, http.StatusNotFound, body)
			return true
		}
		payload := map[string]any{
			"ok":         true,
			"export_job": buildReasoningExportJobPublic(job),
		}
		for key, value := range buildReasoningBehaviorMetadata(runtime) {
			payload[key] = value
		}
		body, _ := json.Marshal(payload)
		writeJSONResponse(writer, http.StatusOK, body)
		return true
	}
	if len(jobParts) == 2 && jobParts[1] == "download" {
		if job == nil || job.Status != "completed" || strings.TrimSpace(job.OutputPath) == "" {
			body, _ := json.Marshal(map[string]any{"ok": false, "error": map[string]any{"type": "not_found", "code": "reasoning_export_job_not_ready", "message": "reasoning 导出任务尚未完成或文件不存在。"}})
			writeJSONResponse(writer, http.StatusNotFound, body)
			return true
		}
		content, err := os.ReadFile(job.OutputPath)
		if err != nil {
			writeGatewayError(writer, err)
			return true
		}
		contentType := "application/json; charset=utf-8"
		fileSuffix := ".json"
		if job.Format == "csv" {
			contentType = "text/csv; charset=utf-8"
			fileSuffix = ".csv"
		}
		filename := fmt.Sprintf("reasoning-export-%s-%s%s", firstNonEmpty(job.DateFrom, "all"), firstNonEmpty(job.DateTo, "all"), fileSuffix)
		writer.Header().Set("content-type", contentType)
		writer.Header().Set("cache-control", "no-store, max-age=0")
		writer.Header().Set("pragma", "no-cache")
		writer.Header().Set("content-disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(content)
		return true
	}
	return false
}

func (runtime *appRuntime) writeStatusResponse(writer http.ResponseWriter, statusCode int, message string) {
	payload := runtime.buildStatusPayload()
	if strings.TrimSpace(message) != "" && message != "ok" {
		payload["message"] = message
	}
	body, _ := json.Marshal(payload)
	writeJSONResponse(writer, statusCode, body)
}

func (runtime *appRuntime) buildStatusPayload() map[string]any {
	if runtime.Monitor == nil {
		runtime.Monitor = newMonitor()
	}
	if runtime.ReasoningBehavior == nil {
		runtime.ReasoningBehavior = newReasoningBehaviorState()
	}
	if runtime.ActiveProbe == nil {
		runtime.ActiveProbe = newActiveProbeMonitor()
	}
	state, _ := readRuntimeState(runtime.Paths)
	return map[string]any{
		"ok":                 true,
		"listen":             fmt.Sprintf("%s:%d", runtime.Config.ListenHost, runtime.Config.ListenPort),
		"config":             runtime.Config,
		"state":              state,
		"paths":              map[string]any{"config_path": runtime.ConfigPath, "state_path": runtime.Paths.StatePath, "state_root": runtime.Paths.StateRoot, "log_path": runtime.LogPath},
		"metrics":            buildMetricsSnapshot(runtime.Monitor),
		"reasoning_behavior": buildReasoningBehaviorRuntimeSnapshot(runtime),
		"model_insights":     buildModelInsightsSnapshot(runtime),
		"active_probe":       buildActiveProbeSnapshot(runtime),
	}
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
	scheduleActiveProbes(runtime)
	runtime.Logger(fmt.Sprintf("[config] updated intercept_rule_mode=%s reasoning_match_mode=%s stream_action=%s retry_upstream_capacity_errors=%t endpoints=%s", nextConfig.InterceptRuleMode, nextConfig.ReasoningMatchMode, nextConfig.StreamAction, nextConfig.RetryUpstreamCapacityErrors, strings.Join(nextConfig.Endpoints, ",")))
	runtime.writeStatusResponse(writer, http.StatusOK, "配置已保存并立即生效")
	return true
}

func (runtime *appRuntime) handleReasoningExport(writer http.ResponseWriter, request *http.Request) bool {
	format := strings.TrimSpace(strings.ToLower(request.URL.Query().Get("format")))
	if format == "" {
		format = "json"
	}
	dateFrom := normalizeDateKeyInput(request.URL.Query().Get("date_from"))
	dateTo := normalizeDateKeyInput(request.URL.Query().Get("date_to"))
	if rangeDays, ok := countInclusiveDateRangeDays(dateFrom, dateTo); ok && rangeDays >= reasoningBehaviorBackgroundExportMinDays {
		job := startReasoningExportJob(runtime, format, dateFrom, dateTo)
		payload := map[string]any{
			"ok":                true,
			"date_from":         nullIfEmpty(dateFrom),
			"date_to":           nullIfEmpty(dateTo),
			"background_export": true,
			"message":           "已创建后台导出任务，可以继续正常使用 gateway。",
			"export_job":        buildReasoningExportJobPublic(job),
		}
		for key, value := range buildReasoningBehaviorMetadata(runtime) {
			payload[key] = value
		}
		body, _ := json.Marshal(payload)
		writeJSONResponse(writer, http.StatusAccepted, body)
		return true
	}

	samples, err := readReasoningBehaviorSamplesByDateRange(runtime, dateFrom, dateTo)
	if err != nil {
		writeGatewayError(writer, err)
		return true
	}
	snapshot := buildReasoningBehaviorSnapshotFromSamples(samples, minInt(len(samples), 200))
	if format == "csv" {
		writer.Header().Set("content-type", "text/csv; charset=utf-8")
		writer.Header().Set("cache-control", "no-store, max-age=0")
		writer.Header().Set("pragma", "no-cache")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(buildReasoningBehaviorCSV(samples)))
		return true
	}

	payload := map[string]any{
		"ok":          true,
		"exported_at": time.Now().Format(time.RFC3339),
		"date_from":   nullIfEmpty(dateFrom),
		"date_to":     nullIfEmpty(dateTo),
		"samples":     samples,
	}
	for key, value := range buildReasoningBehaviorMetadata(runtime) {
		payload[key] = value
	}
	for key, value := range snapshot {
		payload[key] = value
	}
	body, _ := json.Marshal(payload)
	writeJSONResponse(writer, http.StatusOK, body)
	return true
}
