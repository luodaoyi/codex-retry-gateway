package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	historicalImportJobLimit         = 5
	historicalImportSessionFileLimit = 2000
)

type historicalImportState struct {
	NextJobSequence int
	Jobs            map[string]*historicalImportJob
	LastSummary     map[string]any
}

type historicalImportSource struct {
	SourceType string `json:"source_type"`
	Path       string `json:"path"`
	Status     string `json:"status"`
	RowCount   int    `json:"row_count,omitempty"`
	Columns    []any  `json:"columns,omitempty"`
}

type historicalImportJob struct {
	JobID            string                   `json:"job_id"`
	Status           string                   `json:"status"`
	CreatedAt        string                   `json:"created_at"`
	StartedAt        string                   `json:"started_at,omitempty"`
	FinishedAt       string                   `json:"finished_at,omitempty"`
	ErrorMessage     string                   `json:"error_message,omitempty"`
	OutputPath       string                   `json:"output_path,omitempty"`
	Sources          []historicalImportSource `json:"sources"`
	TotalSources     int                      `json:"total_sources"`
	ProcessedSources int                      `json:"processed_sources"`
	CurrentStep      string                   `json:"current_step"`
	Summary          map[string]any           `json:"summary,omitempty"`
	Preflight        map[string]any           `json:"preflight,omitempty"`
	FeatureAnalysis  map[string]any           `json:"feature_analysis,omitempty"`
	Result           map[string]any           `json:"result,omitempty"`
}

func newHistoricalImportState() *historicalImportState {
	return &historicalImportState{
		NextJobSequence: 1,
		Jobs:            map[string]*historicalImportJob{},
	}
}

func buildHistoricalImportSources(payload map[string]any) []historicalImportSource {
	sourcePaths, _ := payload["source_paths"].(map[string]any)
	hasRequestedSources := len(sourcePaths) > 0
	push := func(result *[]historicalImportSource, sourceType string, rawPath any) {
		pathValue := strings.TrimSpace(anyToString(rawPath))
		if pathValue == "" {
			return
		}
		status := "missing"
		if _, err := os.Stat(pathValue); err == nil {
			status = "pending"
		}
		*result = append(*result, historicalImportSource{
			SourceType: sourceType,
			Path:       pathValue,
			Status:     status,
		})
	}
	result := make([]historicalImportSource, 0)
	home := os.Getenv("USERPROFILE")
	if home == "" {
		home = os.Getenv("HOME")
	}
	push(&result, "cc_switch_sqlite", firstNonNil(sourcePaths["cc_switch_db"], sourcePaths["cc_switch_sqlite"], defaultHistoricalPath(home, hasRequestedSources, ".cc-switch", "cc-switch.db")))
	push(&result, "codex_logs_sqlite", firstNonNil(sourcePaths["codex_logs_db"], sourcePaths["codex_logs_sqlite"], defaultHistoricalPath(home, hasRequestedSources, ".codex", "sqlite", "logs_2.sqlite")))
	push(&result, "codex_logs_sqlite", firstNonNil(sourcePaths["codex_logs_db_alt"], defaultHistoricalPath(home, hasRequestedSources, ".codex", "logs_2.sqlite")))
	push(&result, "codex_sessions_jsonl", firstNonNil(sourcePaths["codex_sessions_root"], sourcePaths["codex_sessions_jsonl"], defaultHistoricalPath(home, hasRequestedSources, ".codex", "sessions")))
	seen := map[string]bool{}
	deduped := make([]historicalImportSource, 0, len(result))
	for _, source := range result {
		key := strings.ToLower(source.SourceType + "|" + source.Path)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, source)
	}
	return deduped
}

func defaultHistoricalPath(home string, hasRequestedSources bool, parts ...string) any {
	if hasRequestedSources || home == "" {
		return nil
	}
	allParts := append([]string{home}, parts...)
	return filepath.Join(allParts...)
}

func buildHistoricalImportPreflight(sources []historicalImportSource) (map[string]any, map[string]any) {
	result := buildHistoricalImportPreflightResult(sources)
	preflight, _ := result["preflight"].(map[string]any)
	featureAnalysis, _ := result["feature_analysis"].(map[string]any)
	return preflight, featureAnalysis
}

func createEmptyHistoricalImportResult(sourceCount int) map[string]any {
	return map[string]any{
		"summary": map[string]any{
			"source_count":        sourceCount,
			"total_requests":      0,
			"successful_requests": 0,
			"failed_requests":     0,
			"total_input_tokens":  0,
			"total_output_tokens": 0,
			"avg_latency_ms":      0,
			"codex_log_rows":      0,
			"session_file_count":  0,
			"session_total_bytes": 0,
		},
		"sources": []historicalImportSource{},
		"cc_switch": map[string]any{
			"by_model":     []any{},
			"by_status":    []any{},
			"by_provider":  []any{},
			"recent_daily": []any{},
		},
		"codex_logs": map[string]any{
			"by_level":     []any{},
			"by_target":    []any{},
			"keyword_hits": []any{},
		},
		"sessions": map[string]any{
			"file_count":  0,
			"total_bytes": 0,
			"top_files":   []any{},
		},
		"analysis_samples": []reasoningSample{},
	}
}

func sqliteJSONRows(databasePath string, sql string) ([]map[string]any, error) {
	database, err := sqlOpenSQLite(databasePath)
	if err != nil {
		return nil, err
	}
	defer database.Close()

	rows, err := database.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		row := map[string]any{}
		for index, column := range columns {
			row[column] = normalizeSQLiteValue(values[index])
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func sqlOpenSQLite(databasePath string) (*sql.DB, error) {
	return sql.Open("sqlite", databasePath)
}

func normalizeSQLiteValue(value any) any {
	switch typed := value.(type) {
	case []byte:
		return string(typed)
	default:
		return typed
	}
}

func sqliteJSONRowsSafe(databasePath string, sql string) []map[string]any {
	rows, err := sqliteJSONRows(databasePath, sql)
	if err != nil {
		return []map[string]any{}
	}
	return rows
}

func sqliteTableColumns(databasePath string, tableName string) []any {
	rows := sqliteJSONRowsSafe(databasePath, fmt.Sprintf("PRAGMA table_info(%s);", tableName))
	columns := make([]any, 0, len(rows))
	for _, row := range rows {
		if name := strings.TrimSpace(anyToString(row["name"])); name != "" {
			columns = append(columns, name)
		}
	}
	return columns
}

func numberFromRow(row map[string]any, key string) float64 {
	if row == nil {
		return 0
	}
	return asFloat(row[key])
}

func intFromRow(row map[string]any, key string) int {
	return int(numberFromRow(row, key))
}

func stringSetFromAnySlice(values []any) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		text := strings.ToLower(strings.TrimSpace(anyToString(value)))
		if text != "" {
			result[text] = true
		}
	}
	return result
}

func columnsIncludeAny(columnSet map[string]bool, aliases []string) bool {
	for _, alias := range aliases {
		if columnSet[strings.ToLower(alias)] {
			return true
		}
	}
	return false
}

func buildFieldCoverageFromSQLColumns(columnRows []map[string]any) map[string]any {
	totalRows := 0
	coveredRows := map[string]int{}
	for _, field := range reasoningAnalysisFields {
		coveredRows[field] = 0
	}
	aliases := map[string][]string{
		"reasoning_tokens":    {"reasoning_tokens", "output_reasoning_tokens"},
		"final_answer_only":   {"final_answer_only"},
		"commentary_observed": {"commentary_observed", "has_commentary"},
		"duration_total_ms":   {"duration_total_ms", "duration_ms", "latency_ms"},
		"output_tokens":       {"output_tokens", "completion_tokens", "total_tokens"},
		"model_family":        {"model_family", "effective_local_model_family", "model", "request_model"},
		"reasoning_effort":    {"reasoning_effort", "request_reasoning_effort"},
		"status":              {"status_code", "client_http_status", "upstream_http_status", "final_action"},
		"retry_status":        {"retry_count", "internal_retry_attempt_index", "internal_retry_remaining"},
		"blocked_status":      {"blocked_by_gateway"},
	}
	for _, row := range columnRows {
		rowCount := intFromRow(row, "row_count")
		totalRows += rowCount
		columns, _ := row["columns"].([]any)
		columnSet := stringSetFromAnySlice(columns)
		for _, field := range reasoningAnalysisFields {
			fieldAliases := aliases[field]
			if len(fieldAliases) == 0 {
				fieldAliases = []string{field}
			}
			if columnsIncludeAny(columnSet, fieldAliases) {
				coveredRows[field] += rowCount
			}
		}
	}
	coverage := map[string]any{}
	for _, field := range reasoningAnalysisFields {
		if totalRows == 0 {
			coverage[field] = 0
		} else {
			coverage[field] = roundMetric(float64(coveredRows[field])/float64(totalRows), 6)
		}
	}
	return coverage
}

func cloneHistoricalSource(source historicalImportSource, status string, rowCount int, columns []any) historicalImportSource {
	source.Status = status
	if rowCount > 0 {
		source.RowCount = rowCount
	}
	if columns != nil {
		source.Columns = columns
	}
	return source
}

func addHistoricalSummary(base map[string]any, part map[string]any) {
	for key, value := range part {
		switch typed := value.(type) {
		case int:
			base[key] = roundMetric(asFloat(base[key])+float64(typed), 2)
		case int64:
			base[key] = roundMetric(asFloat(base[key])+float64(typed), 2)
		case float64:
			base[key] = roundMetric(asFloat(base[key])+typed, 2)
		case float32:
			base[key] = roundMetric(asFloat(base[key])+float64(typed), 2)
		}
	}
}

func preflightCcSwitchSource(source historicalImportSource) map[string]any {
	if _, err := os.Stat(source.Path); err != nil {
		return map[string]any{
			"source":     cloneHistoricalSource(source, "missing", 0, nil),
			"summary":    map[string]any{},
			"column_row": map[string]any{"row_count": 0, "columns": []any{}},
		}
	}
	columns := sqliteTableColumns(source.Path, "proxy_request_logs")
	summaryRows := sqliteJSONRowsSafe(source.Path,
		"SELECT count(*) AS total_requests, "+
			"sum(CASE WHEN status_code >= 200 AND status_code < 400 THEN 1 ELSE 0 END) AS successful_requests, "+
			"sum(CASE WHEN status_code >= 400 OR status_code IS NULL THEN 1 ELSE 0 END) AS failed_requests, "+
			"sum(COALESCE(input_tokens,0)) AS total_input_tokens, "+
			"sum(COALESCE(output_tokens,0)) AS total_output_tokens, "+
			"avg(COALESCE(duration_ms, latency_ms)) AS avg_latency_ms "+
			"FROM proxy_request_logs;")
	summary := map[string]any{}
	if len(summaryRows) > 0 {
		summary = summaryRows[0]
	}
	rowCount := intFromRow(summary, "total_requests")
	return map[string]any{
		"source": cloneHistoricalSource(source, "preflight_completed", rowCount, columns),
		"summary": map[string]any{
			"total_requests":      rowCount,
			"successful_requests": intFromRow(summary, "successful_requests"),
			"failed_requests":     intFromRow(summary, "failed_requests"),
			"total_input_tokens":  intFromRow(summary, "total_input_tokens"),
			"total_output_tokens": intFromRow(summary, "total_output_tokens"),
			"avg_latency_ms":      roundMetric(numberFromRow(summary, "avg_latency_ms"), 2),
		},
		"column_row": map[string]any{"row_count": rowCount, "columns": columns},
	}
}

func preflightCodexLogsSource(source historicalImportSource) map[string]any {
	if _, err := os.Stat(source.Path); err != nil {
		return map[string]any{
			"source":       cloneHistoricalSource(source, "missing", 0, nil),
			"summary":      map[string]any{},
			"keyword_hits": []map[string]any{},
		}
	}
	columns := sqliteTableColumns(source.Path, "logs")
	totalRows := sqliteJSONRowsSafe(source.Path, "SELECT count(*) AS row_count FROM logs;")
	keywordHits := sqliteJSONRowsSafe(source.Path,
		"SELECT 'reasoning_tokens' AS keyword, count(*) AS count FROM logs WHERE feedback_log_body LIKE '%reasoning_tokens%' "+
			"UNION ALL SELECT 'final_answer', count(*) FROM logs WHERE feedback_log_body LIKE '%final_answer%' "+
			"UNION ALL SELECT 'commentary', count(*) FROM logs WHERE feedback_log_body LIKE '%commentary%' "+
			"UNION ALL SELECT '502', count(*) FROM logs WHERE feedback_log_body LIKE '%502%';")
	rowCount := 0
	if len(totalRows) > 0 {
		rowCount = intFromRow(totalRows[0], "row_count")
	}
	return map[string]any{
		"source":       cloneHistoricalSource(source, "preflight_completed", rowCount, columns),
		"summary":      map[string]any{"codex_log_rows": rowCount},
		"keyword_hits": keywordHits,
	}
}

func walkSessionFiles(rootPath string, limit int) []map[string]any {
	files := []map[string]any{}
	filepath.WalkDir(rootPath, func(pathValue string, entry os.DirEntry, err error) error {
		if err != nil || len(files) >= limit {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		files = append(files, map[string]any{
			"path":        pathValue,
			"bytes":       info.Size(),
			"modified_at": info.ModTime().UTC().Format(time.RFC3339),
		})
		return nil
	})
	return files
}

func preflightSessionSource(source historicalImportSource) map[string]any {
	if _, err := os.Stat(source.Path); err != nil {
		return map[string]any{
			"source":   cloneHistoricalSource(source, "missing", 0, nil),
			"summary":  map[string]any{},
			"sessions": map[string]any{"file_count": 0, "total_bytes": 0, "top_files": []map[string]any{}},
		}
	}
	files := walkSessionFiles(source.Path, historicalImportSessionFileLimit)
	totalBytes := int64(0)
	for _, file := range files {
		totalBytes += int64(asFloat(file["bytes"]))
	}
	topFiles := append([]map[string]any{}, files...)
	sort.Slice(topFiles, func(left int, right int) bool {
		return asFloat(topFiles[left]["bytes"]) > asFloat(topFiles[right]["bytes"])
	})
	if len(topFiles) > 20 {
		topFiles = topFiles[:20]
	}
	return map[string]any{
		"source": cloneHistoricalSource(source, "preflight_completed", len(files), nil),
		"summary": map[string]any{
			"session_file_count":  len(files),
			"session_total_bytes": totalBytes,
		},
		"sessions": map[string]any{
			"file_count":         len(files),
			"total_bytes":        totalBytes,
			"scanned_file_limit": historicalImportSessionFileLimit,
			"top_files":          topFiles,
		},
	}
}

func buildHistoricalFeatureAnalysisFromPreflight(preflight map[string]any) map[string]any {
	analysisValue := anyToString(preflight["analysis_value"])
	conclusion := "no_analysis_value"
	if analysisValue == "valuable" {
		conclusion = "not_observed"
	} else if analysisValue == "partial" {
		conclusion = "insufficient_fields"
	}
	decisionReason := anyToString(preflight["decision_reason"])
	if strings.TrimSpace(decisionReason) == "" {
		decisionReason = "历史数据缺少 reasoning 行为分析字段。"
	}
	return map[string]any{
		"ok":                  true,
		"analysis_profile":    reasoningAnalysisProfileName,
		"analysis_value":      firstNonEmpty(analysisValue, "no_analysis_value"),
		"conclusion":          conclusion,
		"field_coverage":      firstNonNil(preflight["field_coverage"], map[string]any{}),
		"missing_core_fields": firstNonNil(preflight["missing_core_fields"], []string{}),
		"decision_reason":     decisionReason,
		"candidate_summary":   map[string]any{"candidate_count": 0, "candidate_ratio": 0},
		"baseline_comparison": map[string]any{"baseline_count": 0},
		"samples_preview":     []any{},
	}
}

func buildHistoricalImportPreflightResult(sources []historicalImportSource) map[string]any {
	result := createEmptyHistoricalImportResult(len(sources))
	columnRows := []map[string]any{}
	keywordHits := []map[string]any{}
	resultSources := []historicalImportSource{}
	summary, _ := result["summary"].(map[string]any)
	for _, source := range sources {
		var part map[string]any
		switch source.SourceType {
		case "cc_switch_sqlite":
			part = preflightCcSwitchSource(source)
			if columnRow, ok := part["column_row"].(map[string]any); ok {
				columnRows = append(columnRows, columnRow)
			}
		case "codex_logs_sqlite":
			part = preflightCodexLogsSource(source)
			if hits, ok := part["keyword_hits"].([]map[string]any); ok {
				keywordHits = append(keywordHits, hits...)
			}
		case "codex_sessions_jsonl":
			part = preflightSessionSource(source)
			if sessions, ok := part["sessions"].(map[string]any); ok {
				result["sessions"] = sessions
			}
		default:
			part = map[string]any{"source": cloneHistoricalSource(source, "skipped", 0, nil), "summary": map[string]any{}}
		}
		if partSource, ok := part["source"].(historicalImportSource); ok {
			resultSources = append(resultSources, partSource)
		}
		if partSummary, ok := part["summary"].(map[string]any); ok {
			addHistoricalSummary(summary, partSummary)
		}
	}
	result["sources"] = resultSources
	if codexLogs, ok := result["codex_logs"].(map[string]any); ok {
		codexLogs["keyword_hits"] = keywordHits
	}
	fieldCoverage := buildFieldCoverageFromSQLColumns(columnRows)
	totalRows := 0
	for _, row := range columnRows {
		totalRows += intFromRow(row, "row_count")
	}
	valueDecision := decideReasoningAnalysisValue(fieldCoverage, totalRows)
	preflight := map[string]any{
		"analysis_value":               valueDecision["analysis_value"],
		"can_build_reasoning_features": valueDecision["can_build_reasoning_features"],
		"can_build_candidate_patterns": valueDecision["can_build_candidate_patterns"],
		"field_coverage":               fieldCoverage,
		"missing_core_fields":          valueDecision["missing_core_fields"],
		"decision_reason":              valueDecision["decision_reason"],
		"sources":                      resultSources,
	}
	result["preflight"] = preflight
	result["feature_analysis"] = buildHistoricalFeatureAnalysisFromPreflight(preflight)
	return result
}

func buildHistoricalFeatureAnalysisFromJob(job *historicalImportJob, payload map[string]any) map[string]any {
	if job == nil {
		return buildHistoricalFeatureAnalysisFromPreflight(map[string]any{
			"analysis_value":      "no_analysis_value",
			"field_coverage":      map[string]any{},
			"missing_core_fields": reasoningAnalysisCoreFields,
			"decision_reason":     "没有可分析的历史导入任务。",
		})
	}
	if job.Result != nil {
		if featureAnalysis, ok := job.Result["feature_analysis"].(map[string]any); ok {
			return featureAnalysis
		}
	}
	if job.FeatureAnalysis != nil {
		return job.FeatureAnalysis
	}
	preflight := job.Preflight
	if job.Result != nil {
		if typed, ok := job.Result["preflight"].(map[string]any); ok {
			preflight = typed
		}
	}
	if preflight == nil || anyToString(preflight["analysis_value"]) != "valuable" {
		return buildHistoricalFeatureAnalysisFromPreflight(preflight)
	}
	samples := []reasoningSample{}
	if job.Result != nil {
		samples, _ = job.Result["analysis_samples"].([]reasoningSample)
	}
	return buildFeatureAnalysisFromSamples(samples, buildReasoningAnalysisProfile(payload, "historical_import"))
}

func analyzeCcSwitchDatabase(source historicalImportSource) (map[string]any, error) {
	if _, err := os.Stat(source.Path); err != nil {
		return map[string]any{
			"source":  cloneHistoricalSource(source, "missing", 0, nil),
			"summary": map[string]any{"total_requests": 0, "successful_requests": 0, "failed_requests": 0},
			"cc_switch": map[string]any{
				"by_model":     []map[string]any{},
				"by_status":    []map[string]any{},
				"by_provider":  []map[string]any{},
				"recent_daily": []map[string]any{},
			},
		}, nil
	}
	countRows, err := sqliteJSONRows(source.Path,
		"SELECT count(*) AS total_requests, "+
			"sum(CASE WHEN status_code >= 200 AND status_code < 400 THEN 1 ELSE 0 END) AS successful_requests, "+
			"sum(CASE WHEN status_code >= 400 OR status_code IS NULL THEN 1 ELSE 0 END) AS failed_requests, "+
			"sum(COALESCE(input_tokens,0)) AS total_input_tokens, "+
			"sum(COALESCE(output_tokens,0)) AS total_output_tokens, "+
			"avg(COALESCE(duration_ms, latency_ms)) AS avg_latency_ms "+
			"FROM proxy_request_logs;")
	if err != nil {
		return nil, err
	}
	byModel, err := sqliteJSONRows(source.Path,
		"SELECT COALESCE(NULLIF(model,''), NULLIF(request_model,''), 'unknown') AS model, "+
			"count(*) AS count, "+
			"sum(CASE WHEN status_code >= 200 AND status_code < 400 THEN 1 ELSE 0 END) AS success_count, "+
			"sum(CASE WHEN status_code >= 400 OR status_code IS NULL THEN 1 ELSE 0 END) AS failure_count, "+
			"sum(COALESCE(input_tokens,0)) AS input_tokens, "+
			"sum(COALESCE(output_tokens,0)) AS output_tokens, "+
			"avg(COALESCE(duration_ms, latency_ms)) AS avg_duration_ms "+
			"FROM proxy_request_logs GROUP BY model ORDER BY count DESC LIMIT 20;")
	if err != nil {
		return nil, err
	}
	byStatus, err := sqliteJSONRows(source.Path,
		"SELECT COALESCE(status_code, -1) AS status_code, count(*) AS count "+
			"FROM proxy_request_logs GROUP BY status_code ORDER BY count DESC LIMIT 20;")
	if err != nil {
		return nil, err
	}
	byProvider, err := sqliteJSONRows(source.Path,
		"SELECT COALESCE(NULLIF(provider_id,''), NULLIF(provider_type,''), 'unknown') AS provider, "+
			"count(*) AS count, avg(COALESCE(duration_ms, latency_ms)) AS avg_duration_ms "+
			"FROM proxy_request_logs GROUP BY provider ORDER BY count DESC LIMIT 20;")
	if err != nil {
		return nil, err
	}
	recentDaily, err := sqliteJSONRows(source.Path,
		"SELECT substr(created_at, 1, 10) AS date, count(*) AS count, "+
			"sum(COALESCE(input_tokens,0)) AS input_tokens, sum(COALESCE(output_tokens,0)) AS output_tokens, "+
			"avg(COALESCE(duration_ms, latency_ms)) AS avg_duration_ms "+
			"FROM proxy_request_logs WHERE created_at IS NOT NULL GROUP BY date ORDER BY date DESC LIMIT 31;")
	if err != nil {
		return nil, err
	}
	summary := map[string]any{}
	if len(countRows) > 0 {
		summary = countRows[0]
	}
	rowCount := intFromRow(summary, "total_requests")
	return map[string]any{
		"source": cloneHistoricalSource(source, "completed", rowCount, nil),
		"summary": map[string]any{
			"total_requests":      rowCount,
			"successful_requests": intFromRow(summary, "successful_requests"),
			"failed_requests":     intFromRow(summary, "failed_requests"),
			"total_input_tokens":  intFromRow(summary, "total_input_tokens"),
			"total_output_tokens": intFromRow(summary, "total_output_tokens"),
			"avg_latency_ms":      roundMetric(numberFromRow(summary, "avg_latency_ms"), 2),
		},
		"cc_switch": map[string]any{
			"by_model":     byModel,
			"by_status":    byStatus,
			"by_provider":  byProvider,
			"recent_daily": recentDaily,
		},
	}, nil
}

func analyzeCodexLogsDatabase(source historicalImportSource) (map[string]any, error) {
	if _, err := os.Stat(source.Path); err != nil {
		return map[string]any{
			"source":  cloneHistoricalSource(source, "missing", 0, nil),
			"summary": map[string]any{"codex_log_rows": 0},
			"codex_logs": map[string]any{
				"by_level":     []map[string]any{},
				"by_target":    []map[string]any{},
				"keyword_hits": []map[string]any{},
			},
		}, nil
	}
	totalRows, err := sqliteJSONRows(source.Path, "SELECT count(*) AS row_count FROM logs;")
	if err != nil {
		return nil, err
	}
	byLevel, err := sqliteJSONRows(source.Path,
		"SELECT COALESCE(NULLIF(level,''), 'unknown') AS level, count(*) AS count "+
			"FROM logs GROUP BY level ORDER BY count DESC LIMIT 20;")
	if err != nil {
		return nil, err
	}
	byTarget, err := sqliteJSONRows(source.Path,
		"SELECT COALESCE(NULLIF(target,''), 'unknown') AS target, count(*) AS count "+
			"FROM logs GROUP BY target ORDER BY count DESC LIMIT 20;")
	if err != nil {
		return nil, err
	}
	keywordRows, err := sqliteJSONRows(source.Path,
		"SELECT 'reasoning_tokens' AS keyword, count(*) AS count FROM logs WHERE feedback_log_body LIKE '%reasoning_tokens%' "+
			"UNION ALL SELECT 'final_answer', count(*) FROM logs WHERE feedback_log_body LIKE '%final_answer%' "+
			"UNION ALL SELECT 'commentary', count(*) FROM logs WHERE feedback_log_body LIKE '%commentary%' "+
			"UNION ALL SELECT '502', count(*) FROM logs WHERE feedback_log_body LIKE '%502%';")
	if err != nil {
		return nil, err
	}
	rowCount := 0
	if len(totalRows) > 0 {
		rowCount = intFromRow(totalRows[0], "row_count")
	}
	return map[string]any{
		"source":  cloneHistoricalSource(source, "completed", rowCount, nil),
		"summary": map[string]any{"codex_log_rows": rowCount},
		"codex_logs": map[string]any{
			"by_level":     byLevel,
			"by_target":    byTarget,
			"keyword_hits": keywordRows,
		},
	}, nil
}

func analyzeCodexSessionFiles(source historicalImportSource) map[string]any {
	if _, err := os.Stat(source.Path); err != nil {
		return map[string]any{
			"source":   cloneHistoricalSource(source, "missing", 0, nil),
			"summary":  map[string]any{"session_file_count": 0, "session_total_bytes": 0},
			"sessions": map[string]any{"file_count": 0, "total_bytes": 0, "top_files": []map[string]any{}},
		}
	}
	part := preflightSessionSource(source)
	part["source"] = cloneHistoricalSource(source, "completed", len(walkSessionFiles(source.Path, historicalImportSessionFileLimit)), nil)
	return part
}

func mergeHistoricalImportResult(base map[string]any, part map[string]any) {
	if source, ok := part["source"].(historicalImportSource); ok {
		sources, _ := base["sources"].([]historicalImportSource)
		base["sources"] = append(sources, source)
	}
	if summary, ok := base["summary"].(map[string]any); ok {
		if partSummary, ok := part["summary"].(map[string]any); ok {
			addHistoricalSummary(summary, partSummary)
		}
	}
	if ccSwitch, ok := part["cc_switch"].(map[string]any); ok {
		base["cc_switch"] = ccSwitch
	}
	if codexLogs, ok := part["codex_logs"].(map[string]any); ok {
		baseLogs, _ := base["codex_logs"].(map[string]any)
		if baseLogs == nil {
			base["codex_logs"] = codexLogs
		} else {
			for _, key := range []string{"by_level", "by_target", "keyword_hits"} {
				baseRows, _ := baseLogs[key].([]map[string]any)
				partRows, _ := codexLogs[key].([]map[string]any)
				baseLogs[key] = append(baseRows, partRows...)
			}
		}
	}
	if sessions, ok := part["sessions"].(map[string]any); ok {
		base["sessions"] = sessions
	}
}

func buildHistoricalImportJobPublic(job *historicalImportJob) map[string]any {
	if job == nil {
		return nil
	}
	percent := 0.0
	if job.TotalSources > 0 {
		percent = float64(job.ProcessedSources) / float64(job.TotalSources)
		if percent > 1 {
			percent = 1
		}
	}
	result := job.Result
	if result == nil {
		result = map[string]any{}
	}
	return map[string]any{
		"job_id":        job.JobID,
		"status":        job.Status,
		"created_at":    nullIfEmpty(job.CreatedAt),
		"started_at":    nullIfEmpty(job.StartedAt),
		"finished_at":   nullIfEmpty(job.FinishedAt),
		"error_message": nullIfEmpty(job.ErrorMessage),
		"output_path":   nullIfEmpty(job.OutputPath),
		"progress": map[string]any{
			"total_sources":     job.TotalSources,
			"processed_sources": job.ProcessedSources,
			"percent":           roundMetric(percent, 4),
			"current_step":      job.CurrentStep,
		},
		"summary":          firstNonNil(result["summary"], job.Summary, nil),
		"preflight":        firstNonNil(result["preflight"], job.Preflight, nil),
		"feature_analysis": firstNonNil(result["feature_analysis"], job.FeatureAnalysis, nil),
		"sources":          firstNonNil(result["sources"], []historicalImportSource{}),
		"cc_switch":        firstNonNil(result["cc_switch"], map[string]any{"by_model": []any{}, "by_status": []any{}, "by_provider": []any{}, "recent_daily": []any{}}),
		"codex_logs":       firstNonNil(result["codex_logs"], map[string]any{"by_level": []any{}, "by_target": []any{}, "keyword_hits": []any{}}),
		"sessions":         firstNonNil(result["sessions"], map[string]any{"file_count": 0, "total_bytes": 0, "top_files": []any{}}),
	}
}

func trimHistoricalImportJobs(state *historicalImportState) {
	jobs := make([]*historicalImportJob, 0, len(state.Jobs))
	for _, job := range state.Jobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(left int, right int) bool {
		return jobs[left].CreatedAt > jobs[right].CreatedAt
	})
	if len(jobs) <= historicalImportJobLimit {
		return
	}
	for _, job := range jobs[historicalImportJobLimit:] {
		if job.Status == "running" || job.Status == "queued" {
			continue
		}
		delete(state.Jobs, job.JobID)
	}
}

func writeHistoricalImportSummary(runtime *appRuntime, job *historicalImportJob) (string, error) {
	outputRoot := filepath.Join(runtime.Paths.AnalyticsRoot, "imports", job.JobID)
	if err := os.MkdirAll(outputRoot, 0o755); err != nil {
		return "", err
	}
	outputPath := filepath.Join(outputRoot, "summary.json")
	payload := map[string]any{
		"ok":           true,
		"generated_at": time.Now().Format(time.RFC3339),
		"import_job":   buildHistoricalImportJobPublic(job),
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(outputPath, append(body, '\n'), 0o644); err != nil {
		return "", err
	}
	return outputPath, nil
}

func runHistoricalImportJob(runtime *appRuntime, job *historicalImportJob) {
	runtime.ReasoningMu.Lock()
	job.Status = "running"
	job.StartedAt = time.Now().Format(time.RFC3339)
	job.CurrentStep = "preflight"
	runtime.ReasoningMu.Unlock()

	preflightResult := buildHistoricalImportPreflightResult(job.Sources)
	preflight, _ := preflightResult["preflight"].(map[string]any)
	featureAnalysis, _ := preflightResult["feature_analysis"].(map[string]any)
	result := createEmptyHistoricalImportResult(len(job.Sources))
	result["preflight"] = preflight
	result["feature_analysis"] = featureAnalysis

	var jobErr error
	processedSources := 0
	for index, source := range job.Sources {
		runtime.ReasoningMu.Lock()
		job.CurrentStep = fmt.Sprintf("processing %s", source.SourceType)
		job.ProcessedSources = index
		runtime.ReasoningMu.Unlock()

		var part map[string]any
		switch source.SourceType {
		case "cc_switch_sqlite":
			part, jobErr = analyzeCcSwitchDatabase(source)
		case "codex_logs_sqlite":
			part, jobErr = analyzeCodexLogsDatabase(source)
		case "codex_sessions_jsonl":
			part = analyzeCodexSessionFiles(source)
		default:
			part = map[string]any{"source": cloneHistoricalSource(source, "skipped", 0, nil), "summary": map[string]any{}}
		}
		if jobErr != nil {
			break
		}
		mergeHistoricalImportResult(result, part)
		processedSources++
		runtime.ReasoningMu.Lock()
		job.ProcessedSources = processedSources
		runtime.ReasoningMu.Unlock()
	}

	runtime.ReasoningMu.Lock()
	job.Preflight = preflight
	job.FeatureAnalysis = featureAnalysis
	job.ProcessedSources = processedSources
	job.CurrentStep = "completed"
	if jobErr != nil {
		job.Status = "failed"
		job.ErrorMessage = jobErr.Error()
		job.FinishedAt = time.Now().Format(time.RFC3339)
		runtime.ReasoningMu.Unlock()
		runtime.Logger(fmt.Sprintf("[analytics-error] historical import failed job=%s message=%s", job.JobID, jobErr.Error()))
		return
	}
	if summary, ok := result["summary"].(map[string]any); ok {
		job.Summary = summary
	}
	job.Result = result
	outputPath, err := writeHistoricalImportSummary(runtime, job)
	if err != nil {
		job.Status = "failed"
		job.ErrorMessage = err.Error()
		job.FinishedAt = time.Now().Format(time.RFC3339)
		runtime.ReasoningMu.Unlock()
		runtime.Logger(fmt.Sprintf("[analytics-error] historical import failed job=%s message=%s", job.JobID, err.Error()))
		return
	}
	job.OutputPath = outputPath
	job.Status = "completed"
	job.FinishedAt = time.Now().Format(time.RFC3339)
	runtime.HistoricalImports.LastSummary = buildHistoricalImportJobPublic(job)
	runtime.ReasoningMu.Unlock()
}

func startHistoricalImportJob(runtime *appRuntime, payload map[string]any) *historicalImportJob {
	sources := buildHistoricalImportSources(payload)
	runtime.ReasoningMu.Lock()
	sequence := runtime.HistoricalImports.NextJobSequence
	runtime.HistoricalImports.NextJobSequence++
	job := &historicalImportJob{
		JobID:        fmt.Sprintf("historical_import_%d_%d", time.Now().UnixMilli(), sequence),
		Status:       "queued",
		CreatedAt:    time.Now().Format(time.RFC3339),
		Sources:      sources,
		TotalSources: len(sources),
		CurrentStep:  "queued",
	}
	runtime.HistoricalImports.Jobs[job.JobID] = job
	trimHistoricalImportJobs(runtime.HistoricalImports)
	runtime.ReasoningMu.Unlock()
	go runHistoricalImportJob(runtime, job)
	return job
}

func getHistoricalImportJob(runtime *appRuntime, jobID string) *historicalImportJob {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	job := runtime.HistoricalImports.Jobs[jobID]
	if job == nil {
		return nil
	}
	cloned := *job
	cloned.Sources = append([]historicalImportSource{}, job.Sources...)
	return &cloned
}

func latestHistoricalImportPublic(runtime *appRuntime) map[string]any {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	var latest *historicalImportJob
	for _, job := range runtime.HistoricalImports.Jobs {
		if latest == nil || job.CreatedAt > latest.CreatedAt {
			latest = job
		}
	}
	if latest != nil {
		return buildHistoricalImportJobPublic(latest)
	}
	return runtime.HistoricalImports.LastSummary
}
