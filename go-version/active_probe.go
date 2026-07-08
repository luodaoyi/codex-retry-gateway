package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	longContextProbeFillerUnit       = " a"
	longContextProbeSeedUnitCount    = 8192
	longContextProbeTokenTolerance   = 1024
	longContextProbeMaxBudgetAttempt = 2
	defaultActiveProbeUserAgent      = "codex-retry-gateway/active-probe"
	probeImageDataURL                = "data:image/png;base64," +
		"iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAYAAACqaXHeAAAAAXNSR0IArs4c6QAAAARnQU1BAACxjwv8YQUAAAAJcEhZcwAADsMAAA7DAcdvqGQAAAGwSURBVHhe7ZdRjoMwDEQ5Xg6U4+QuXIWbZLWifHQyZhfIuKrsJ+XHpRV+nkC69OAsWIhGCsBCNFIAFqKRArAQjRSAhWikACxEIwVgIRopAAvRSAFYULO10pdlMVbpbcNvaHEWsPY6NP2+irMBXwFrHRoeVmndU4GrgLWShofluw0cBbD4116JFM9t4CeAxb+uxkOx9hW/L8JNAIt//e1ya70MAl6fOeAkgMd/73HrreBnezo88BFgxP/gk9vAQQCf8NuAP7gN9AJoczhdLsljG8gF0HiTxuh1g6j5iAXwyZL+jaQY105EK4A2ZU2Vy2JpmYlUAI31SUP0evHRWCiAT/SkfyMx2qOxTgB795vxP+DSlP8QZQLY0ff+0m0DkQB29H22VNtAI4DG/+ESbQOJgLnxP5ZmGwgE8PifPv0Rx7fBfAE0/n89/RG/t8FkAcaNXxr/Dj8UXUzSP5grwIjurZs2fuuOzDOmCuBTuxr/AyNNt3+PM1GAccMPJsaF3kyUwUQB30kKwEI0UgAWopECsBCNFICFaKQALEQjBWAhGikAC9FIAViIRgrAQjTCC/gBCi0Q+LleBhsAAAAASUVORK5CYII="
)

type probeSample struct {
	TS                   string     `json:"ts"`
	ProbeType            string     `json:"probe_type"`
	TargetModel          string     `json:"target_model,omitempty"`
	TargetFamily         string     `json:"target_family,omitempty"`
	EndpointPath         string     `json:"endpoint_path,omitempty"`
	Result               string     `json:"result"`
	ResultType           string     `json:"result_type,omitempty"`
	Confidence           string     `json:"confidence,omitempty"`
	HTTPStatus           *int       `json:"http_status,omitempty"`
	DurationMS           int        `json:"duration_ms"`
	ErrorExcerpt         string     `json:"error_excerpt,omitempty"`
	UpstreamModel        string     `json:"upstream_model,omitempty"`
	StreamModel          string     `json:"stream_model,omitempty"`
	FinalResponseModel   string     `json:"final_response_model,omitempty"`
	ObservedModels       []string   `json:"observed_models,omitempty"`
	ObservedFingerprints []string   `json:"observed_fingerprints,omitempty"`
	RequestedInputTokens *int       `json:"requested_input_tokens,omitempty"`
	ObservedInputTokens  *int       `json:"observed_input_tokens,omitempty"`
	EstimatedInputTokens *int       `json:"estimated_input_tokens,omitempty"`
	TokenBudgetSource    string     `json:"token_budget_source,omitempty"`
	CalibrationRounds    *int       `json:"calibration_rounds,omitempty"`
	AttemptCount         int        `json:"attempt_count,omitempty"`
	HTTPStatuses         []int      `json:"http_statuses,omitempty"`
	EvidenceLogs         []logEntry `json:"evidence_logs"`
}

type probeAttempt struct {
	ResponseStatus      *int
	ParsedBody          map[string]any
	RequestError        error
	DurationMS          int
	ResponseText        string
	ResponseBodyExcerpt string
	ModelContext        *requestModelContext
	InputTokens         *int
}

type activeProbeTarget struct {
	Model  string
	Family string
}

type activeProbeMonitor struct {
	mu                     sync.Mutex
	Enabled                bool
	Running                bool
	LastStartedAt          string
	LastFinishedAt         string
	LastTargetModel        string
	LastTargetFamily       string
	TotalRuns              int
	SkippedRuns            int
	PassCount              int
	WarningCount           int
	ViolationCount         int
	TransportErrorCount    int
	IndeterminateCount     int
	EndpointSuccessCounts  map[string]int
	ProbeTypeCounts        map[string]int
	WarningTypeCounts      map[string]int
	ViolationTypeCounts    map[string]int
	LastSuccessfulEndpoint string
	RecentSamples          []probeSample
}

func newActiveProbeMonitor() *activeProbeMonitor {
	return &activeProbeMonitor{
		EndpointSuccessCounts: map[string]int{},
		ProbeTypeCounts: map[string]int{
			"long_context":         0,
			"image_input":          0,
			"response_structure":   0,
			"identity_consistency": 0,
			"knowledge_cutoff":     0,
		},
		WarningTypeCounts: map[string]int{
			"probe_response_structure_warning":   0,
			"probe_identity_consistency_warning": 0,
			"probe_knowledge_cutoff_warning":     0,
		},
		ViolationTypeCounts: map[string]int{
			"probe_low_context_family_violation": 0,
			"probe_image_input_violation":        0,
		},
		RecentSamples: []probeSample{},
	}
}

func cloneStringIntMap(source map[string]int) map[string]int {
	result := map[string]int{}
	for key, value := range source {
		result[key] = value
	}
	return result
}

func buildActiveProbeSnapshot(runtime *appRuntime) map[string]any {
	monitor := runtime.ActiveProbe
	if monitor == nil {
		monitor = newActiveProbeMonitor()
	}
	monitor.mu.Lock()
	defer monitor.mu.Unlock()
	return map[string]any{
		"enabled":                  runtime.Config.ActiveProbe.Enabled,
		"running":                  monitor.Running,
		"interval_ms":              runtime.Config.ActiveProbe.IntervalMS,
		"target_families":          append([]string{}, runtime.Config.ActiveProbe.TargetFamilies...),
		"last_started_at":          nullIfEmpty(monitor.LastStartedAt),
		"last_finished_at":         nullIfEmpty(monitor.LastFinishedAt),
		"last_target_model":        nullIfEmpty(monitor.LastTargetModel),
		"last_target_family":       nullIfEmpty(monitor.LastTargetFamily),
		"total_runs":               monitor.TotalRuns,
		"skipped_runs":             monitor.SkippedRuns,
		"pass_count":               monitor.PassCount,
		"warning_count":            monitor.WarningCount,
		"violation_count":          monitor.ViolationCount,
		"transport_error_count":    monitor.TransportErrorCount,
		"indeterminate_count":      monitor.IndeterminateCount,
		"endpoint_success_counts":  cloneStringIntMap(monitor.EndpointSuccessCounts),
		"probe_type_counts":        cloneStringIntMap(monitor.ProbeTypeCounts),
		"warning_type_counts":      cloneStringIntMap(monitor.WarningTypeCounts),
		"violation_type_counts":    cloneStringIntMap(monitor.ViolationTypeCounts),
		"last_successful_endpoint": nullIfEmpty(monitor.LastSuccessfulEndpoint),
		"recent_samples":           append([]probeSample{}, monitor.RecentSamples...),
		"config":                   runtime.Config.ActiveProbe,
	}
}

func buildTargetModelForFamily(localModel string, targetFamily string) string {
	normalizedFamily := normalizeModelFamily(strings.TrimPrefix(strings.TrimSpace(targetFamily), "/"))
	if !trackedLocalModelFamilies[normalizedFamily] {
		return ""
	}
	localValue := strings.TrimSpace(localModel)
	if localValue != "" && normalizeModelFamily(localValue) == normalizedFamily {
		return localValue
	}
	return normalizedFamily
}

func normalizeTrackedFamilyList(values []string) []string {
	result := []string{}
	for _, value := range values {
		family := normalizeModelFamily(strings.TrimPrefix(strings.TrimSpace(value), "/"))
		if !trackedLocalModelFamilies[family] {
			continue
		}
		seen := false
		for _, existing := range result {
			if existing == family {
				seen = true
				break
			}
		}
		if !seen {
			result = append(result, family)
		}
	}
	return result
}

func extractTopLevelModel(content string) string {
	match := regexp.MustCompile(`(?m)^\s*model\s*=\s*"([^"]+)"\s*$`).FindStringSubmatch(content)
	if match == nil {
		return ""
	}
	return strings.TrimSpace(match[1])
}

func getLocalConfigModel(runtime *appRuntime) string {
	state, err := readInstallState(runtime.Paths.StatePath)
	if err != nil || state == nil || strings.TrimSpace(state.CodexConfigPath) == "" {
		return ""
	}
	content, err := os.ReadFile(state.CodexConfigPath)
	if err != nil {
		return ""
	}
	return extractTopLevelModel(string(content))
}

func resolveActiveProbeTargets(config gatewayConfig, localModel string) []activeProbeTarget {
	selectedFamilies := normalizeTrackedFamilyList(config.ActiveProbe.TargetFamilies)
	if len(selectedFamilies) > 0 {
		targets := make([]activeProbeTarget, 0, len(selectedFamilies))
		for _, family := range selectedFamilies {
			if model := buildTargetModelForFamily(localModel, family); model != "" {
				targets = append(targets, activeProbeTarget{Model: model, Family: family})
			}
		}
		return targets
	}
	localFamily := normalizeModelFamily(localModel)
	if !trackedLocalModelFamilies[localFamily] {
		return []activeProbeTarget{}
	}
	return []activeProbeTarget{{Model: localModel, Family: localFamily}}
}

func chooseProbeEndpoint(config activeProbeConfig) string {
	for _, endpoint := range config.EndpointCandidates {
		if strings.TrimSpace(endpoint) != "" {
			return normalizePath(endpoint)
		}
	}
	return "/responses"
}

type probeClassification struct {
	Result     string
	ResultType string
	Confidence string
}

func activeProbeMapEnabled(configMap map[string]any, fallback bool) bool {
	if configMap == nil {
		return fallback
	}
	return optionalBool(configMap["enabled"], fallback)
}

func activeProbeMapInt(configMap map[string]any, key string, fallback int) int {
	if configMap == nil {
		return fallback
	}
	value, err := parseOptionalInt(configMap[key], fallback)
	if err != nil {
		return fallback
	}
	return value
}

func buildProbeRequestURL(baseURL string, endpointPath string) (string, error) {
	return buildUpstreamURL(baseURL, &url.URL{Path: normalizePath(endpointPath)})
}

func applyActiveProbePayloadProfile(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	cloned := cloneJSONMap(payload)
	reasoning, _ := cloned["reasoning"].(map[string]any)
	if reasoning == nil {
		reasoning = map[string]any{}
	}
	if strings.TrimSpace(anyToString(reasoning["effort"])) == "" {
		reasoning["effort"] = "medium"
	}
	cloned["reasoning"] = reasoning
	return cloned
}

func buildActiveProbeHeaders(runtime *appRuntime) http.Header {
	headers := http.Header{}
	headers.Set("content-type", "application/json; charset=utf-8")
	headers.Set("user-agent", defaultActiveProbeUserAgent)
	state, err := readInstallState(runtime.Paths.StatePath)
	if err != nil || state == nil || strings.TrimSpace(state.CodexConfigPath) == "" || strings.TrimSpace(state.ProviderName) == "" {
		return headers
	}
	codexConfig, err := os.ReadFile(state.CodexConfigPath)
	if err != nil || !extractProviderBooleanSetting(string(codexConfig), state.ProviderName, "requires_openai_auth") {
		return headers
	}
	authPathCandidates := []string{
		filepath.Join(filepath.Dir(state.CodexConfigPath), "auth.json"),
		filepath.Join(runtime.Paths.StateRoot, "auth.json"),
	}
	for _, authPath := range authPathCandidates {
		content, err := os.ReadFile(authPath)
		if err != nil {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(content, &payload) != nil {
			continue
		}
		apiKey := strings.TrimSpace(anyToString(payload["OPENAI_API_KEY"]))
		if apiKey != "" {
			headers.Set("authorization", "Bearer "+apiKey)
			break
		}
	}
	return headers
}

func extractProbeResponseText(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if text := strings.TrimSpace(anyToString(payload["output_text"])); text != "" {
		return text
	}
	if output, ok := payload["output"].([]any); ok {
		parts := []string{}
		for _, item := range output {
			itemMap, _ := item.(map[string]any)
			if content, ok := itemMap["content"].([]any); ok {
				for _, contentItem := range content {
					contentMap, _ := contentItem.(map[string]any)
					if text := strings.TrimSpace(anyToString(contentMap["text"])); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	if choices, ok := payload["choices"].([]any); ok {
		for _, choice := range choices {
			choiceMap, _ := choice.(map[string]any)
			if message, ok := choiceMap["message"].(map[string]any); ok {
				if text := strings.TrimSpace(anyToString(message["content"])); text != "" {
					return text
				}
			}
		}
	}
	return ""
}

func extractProbeBodyExcerpt(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	if errPayload, ok := payload["error"].(map[string]any); ok {
		parts := []string{}
		for _, key := range []string{"type", "code", "message"} {
			if text := strings.TrimSpace(anyToString(errPayload[key])); text != "" {
				parts = append(parts, text)
			}
		}
		if len(parts) > 0 {
			return truncateText(strings.Join(parts, " | "), 500)
		}
	}
	return truncateText(extractProbeResponseText(payload), 500)
}

func looksLikeImageInputUnsupported(payload map[string]any) bool {
	text := strings.ToLower(extractProbeBodyExcerpt(payload))
	for _, marker := range []string{"image", "vision", "input_image", "unsupported", "not support", "does not support"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func looksLikeLongContextLimitError(payload map[string]any) bool {
	text := strings.ToLower(extractProbeBodyExcerpt(payload))
	for _, marker := range []string{"context length", "maximum context", "too many tokens", "context window", "reduce the length"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func classifyImageProbeResult(status *int, parsedBody map[string]any, requestErr error) probeClassification {
	if requestErr != nil {
		return probeClassification{Result: "transport_error"}
	}
	if status != nil && *status >= 500 {
		return probeClassification{Result: "transport_error"}
	}
	if looksLikeImageInputUnsupported(parsedBody) {
		return probeClassification{Result: "violation", ResultType: "probe_image_input_violation", Confidence: "high"}
	}
	if status != nil && *status >= 200 && *status < 300 {
		return probeClassification{Result: "pass", Confidence: "medium"}
	}
	return probeClassification{Result: "indeterminate"}
}

func classifyLongContextProbeResult(status *int, parsedBody map[string]any, requestErr error) probeClassification {
	if requestErr != nil {
		return probeClassification{Result: "transport_error"}
	}
	if status != nil && *status >= 500 {
		return probeClassification{Result: "transport_error"}
	}
	if looksLikeLongContextLimitError(parsedBody) || looksLikeLowContextFamilyError(parsedBody) {
		return probeClassification{Result: "violation", ResultType: "probe_low_context_family_violation", Confidence: "high"}
	}
	if status != nil && *status >= 200 && *status < 300 {
		return probeClassification{Result: "pass", Confidence: "medium"}
	}
	return probeClassification{Result: "indeterminate"}
}

func collectProbeEvidenceLogs(lines []string, probeType string) []logEntry {
	start := 0
	if len(lines) > 4 {
		start = len(lines) - 4
	}
	entries := make([]logEntry, 0, len(lines[start:]))
	for _, line := range lines[start:] {
		entries = append(entries, logEntry{
			At:      time.Now().Format(time.RFC3339),
			Message: fmt.Sprintf("[probe:%s] %s", probeType, line),
		})
	}
	return entries
}

func pushProbeSample(monitor *activeProbeMonitor, sample probeSample) {
	monitor.RecentSamples = append([]probeSample{sample}, monitor.RecentSamples...)
	if len(monitor.RecentSamples) > 50 {
		monitor.RecentSamples = monitor.RecentSamples[:50]
	}
}

func applyProbeResultCounters(monitor *activeProbeMonitor, sample probeSample) {
	monitor.ProbeTypeCounts[sample.ProbeType]++
	switch sample.Result {
	case "pass":
		monitor.PassCount++
	case "warning":
		monitor.WarningCount++
		if sample.ResultType != "" {
			monitor.WarningTypeCounts[sample.ResultType]++
		}
	case "violation":
		monitor.ViolationCount++
		if sample.ResultType != "" {
			monitor.ViolationTypeCounts[sample.ResultType]++
		}
	case "transport_error":
		monitor.TransportErrorCount++
	case "indeterminate":
		monitor.IndeterminateCount++
	}
}

func executeProbeRequest(runtime *appRuntime, probeType string, endpointPath string, payload map[string]any, targetModel string, targetFamily string, classify func(*int, map[string]any, error) probeClassification) probeSample {
	logLines := []string{fmt.Sprintf("[probe] start type=%s family=%s endpoint=%s", probeType, targetFamily, endpointPath)}
	attempt := executeProbeAttempt(runtime, endpointPath, payload, targetModel)
	classified := classify(attempt.ResponseStatus, attempt.ParsedBody, attempt.RequestError)
	errorExcerpt := ""
	if attempt.RequestError != nil {
		errorExcerpt = attempt.RequestError.Error()
	} else {
		errorExcerpt = attempt.ResponseBodyExcerpt
	}
	statusText := "-"
	if attempt.ResponseStatus != nil {
		statusText = fmt.Sprintf("%d", *attempt.ResponseStatus)
	}
	logLines = append(logLines, fmt.Sprintf("[probe] finish type=%s family=%s status=%s result=%s result_type=%s confidence=%s", probeType, targetFamily, statusText, classified.Result, firstNonEmpty(classified.ResultType, "-"), firstNonEmpty(classified.Confidence, "-")))
	if errorExcerpt != "" {
		logLines = append(logLines, fmt.Sprintf("[probe] evidence type=%s family=%s detail=%s", probeType, targetFamily, errorExcerpt))
	}
	return probeSample{
		TS:                   time.Now().Format(time.RFC3339),
		ProbeType:            probeType,
		TargetModel:          targetModel,
		TargetFamily:         targetFamily,
		EndpointPath:         endpointPath,
		Result:               classified.Result,
		ResultType:           classified.ResultType,
		Confidence:           classified.Confidence,
		HTTPStatus:           attempt.ResponseStatus,
		DurationMS:           attempt.DurationMS,
		ErrorExcerpt:         errorExcerpt,
		UpstreamModel:        attempt.ModelContext.UpstreamModel,
		StreamModel:          attempt.ModelContext.StreamModel,
		FinalResponseModel:   attempt.ModelContext.FinalResponseModel,
		ObservedModels:       sortedStringSet(attempt.ModelContext.ObservedModels),
		ObservedFingerprints: sortedStringSet(attempt.ModelContext.ObservedFingerprints),
		EvidenceLogs:         collectProbeEvidenceLogs(logLines, probeType),
	}
}

func executeProbeAttempt(runtime *appRuntime, endpointPath string, payload map[string]any, targetModel string) probeAttempt {
	started := time.Now()
	payload = applyActiveProbePayloadProfile(payload)
	modelContext := createRequestModelContext(targetModel, anyToString(payload["model"]))
	attempt := probeAttempt{ModelContext: modelContext}
	upstreamURL, buildErr := buildProbeRequestURL(runtime.Config.UpstreamBaseURL, endpointPath)
	if buildErr != nil {
		attempt.RequestError = buildErr
		attempt.DurationMS = int(time.Since(started).Milliseconds())
		return attempt
	}
	body, _ := json.Marshal(payload)
	timeout := time.Duration(runtime.Config.ActiveProbe.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		attempt.RequestError = err
		attempt.DurationMS = int(time.Since(started).Milliseconds())
		return attempt
	}
	for key, values := range buildActiveProbeHeaders(runtime) {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		attempt.RequestError = err
		attempt.DurationMS = int(time.Since(started).Milliseconds())
		return attempt
	}
	defer response.Body.Close()
	statusValue := response.StatusCode
	attempt.ResponseStatus = &statusValue
	responseBody, _ := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024))
	if isJSONContentType(response.Header.Get("content-type")) || len(responseBody) > 0 {
		attempt.ParsedBody = parseJSON(responseBody)
	}
	if attempt.ParsedBody != nil {
		applyPayloadModelSignals(modelContext, attempt.ParsedBody, false, true)
	}
	attempt.InputTokens = extractInputTokens(attempt.ParsedBody)
	attempt.ResponseText = extractProbeResponseText(attempt.ParsedBody)
	attempt.ResponseBodyExcerpt = extractProbeBodyExcerpt(attempt.ParsedBody)
	attempt.DurationMS = int(time.Since(started).Milliseconds())
	if endpointPath != "" && runtime.ActiveProbe != nil {
		runtime.ActiveProbe.mu.Lock()
		runtime.ActiveProbe.EndpointSuccessCounts[endpointPath]++
		runtime.ActiveProbe.LastSuccessfulEndpoint = endpointPath
		runtime.ActiveProbe.mu.Unlock()
	}
	return attempt
}

func buildLongContextProbeText(unitCount int, phase string) string {
	if unitCount < 0 {
		unitCount = 0
	}
	filler := ""
	if unitCount > 0 {
		filler = strings.Repeat(longContextProbeFillerUnit, unitCount)
		if strings.HasPrefix(longContextProbeFillerUnit, " ") && len(filler) > 0 {
			filler = filler[1:]
		}
	}
	parts := []string{fmt.Sprintf("__crg_long_context_probe__ phase=%s units=%d", phase, unitCount)}
	if filler != "" {
		parts = append(parts, filler)
	}
	parts = append(parts, "只回复OK")
	return strings.Join(parts, "\n")
}

func buildLongContextProbePayload(targetModel string, unitCount int, phase string) map[string]any {
	return map[string]any{
		"model":             targetModel,
		"max_output_tokens": 4,
		"reasoning":         map[string]any{"effort": "medium"},
		"input": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": buildLongContextProbeText(unitCount, phase)}},
		}},
	}
}

func estimateLongContextUnitCount(baseInputTokens int, measuredInputTokens int, measuredUnitCount int, targetInputTokens int) *int {
	numerator := measuredInputTokens - baseInputTokens
	if numerator <= 0 || measuredUnitCount <= 0 {
		return nil
	}
	tokensPerUnit := float64(numerator) / float64(measuredUnitCount)
	if tokensPerUnit <= 0 {
		return nil
	}
	estimated := max(1, intCeil(float64(targetInputTokens-baseInputTokens)/tokensPerUnit))
	return &estimated
}

func intCeil(value float64) int {
	integer := int(value)
	if value > float64(integer) {
		return integer + 1
	}
	return integer
}

func combineProbeDetail(primary string, secondary string) string {
	parts := []string{}
	for _, value := range []string{primary, secondary} {
		if text := strings.TrimSpace(value); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return truncateText(strings.Join(parts, " | "), 320)
}

func firstIntPointer(values ...*int) *int {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func buildLongContextBudgetDetail(targetInputTokens int, observedInputTokens *int, estimatedInputTokens *int, baselineInputTokens *int, seedInputTokens *int, unitCount *int, calibrationRounds int) string {
	parts := []string{fmt.Sprintf("target_input_tokens=%d", targetInputTokens)}
	if observedInputTokens != nil {
		parts = append(parts, fmt.Sprintf("observed_input_tokens=%d", *observedInputTokens))
	}
	if estimatedInputTokens != nil {
		parts = append(parts, fmt.Sprintf("estimated_input_tokens=%d", *estimatedInputTokens))
	}
	if baselineInputTokens != nil {
		parts = append(parts, fmt.Sprintf("baseline_input_tokens=%d", *baselineInputTokens))
	}
	if seedInputTokens != nil {
		parts = append(parts, fmt.Sprintf("seed_input_tokens=%d", *seedInputTokens))
	}
	if unitCount != nil {
		parts = append(parts, fmt.Sprintf("unit_count=%d", *unitCount))
	}
	parts = append(parts, fmt.Sprintf("calibration_rounds=%d", calibrationRounds), "budget_source=response_usage")
	return strings.Join(parts, " ")
}

func parseProbeJSONText(text string) map[string]any {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil
	}
	var parsed map[string]any
	if json.Unmarshal([]byte(trimmed), &parsed) == nil {
		return parsed
	}
	firstBrace := strings.Index(trimmed, "{")
	lastBrace := strings.LastIndex(trimmed, "}")
	if firstBrace < 0 || lastBrace <= firstBrace {
		return nil
	}
	if json.Unmarshal([]byte(trimmed[firstBrace:lastBrace+1]), &parsed) == nil {
		return parsed
	}
	return nil
}

func isExpectedResponseStructurePayload(parsed map[string]any) bool {
	items, ok := parsed["items"].([]any)
	if !ok || len(items) != 3 {
		return false
	}
	expectedKeys := []string{"a", "b", "c"}
	expectedValues := []float64{1, 2, 3}
	for index, item := range items {
		itemMap, _ := item.(map[string]any)
		if anyToString(itemMap["key"]) != expectedKeys[index] || asFloat(itemMap["value"]) != expectedValues[index] {
			return false
		}
	}
	return true
}

func classifyResponseStructureProbeResult(attempts []probeAttempt) probeClassification {
	for _, attempt := range attempts {
		if attempt.RequestError != nil || (attempt.ResponseStatus != nil && *attempt.ResponseStatus >= 500) {
			return probeClassification{Result: "transport_error"}
		}
	}
	invalidCount := 0
	for _, attempt := range attempts {
		text := strings.TrimSpace(attempt.ResponseText)
		parsed := parseProbeJSONText(text)
		exact := map[string]any(nil)
		if json.Unmarshal([]byte(text), &exact) != nil {
			exact = nil
		}
		hasExtraText := text != "" && exact == nil && parsed != nil
		if text == "" || parsed == nil || !isExpectedResponseStructurePayload(parsed) || hasExtraText {
			invalidCount++
		}
	}
	if invalidCount >= 2 {
		return probeClassification{Result: "warning", ResultType: "probe_response_structure_warning", Confidence: "medium"}
	}
	if invalidCount == 0 {
		return probeClassification{Result: "pass", Confidence: "medium"}
	}
	return probeClassification{Result: "indeterminate"}
}

func classifyIdentityConsistencyProbeResult(attempts []probeAttempt) probeClassification {
	for _, attempt := range attempts {
		if attempt.RequestError != nil || (attempt.ResponseStatus != nil && *attempt.ResponseStatus >= 500) {
			return probeClassification{Result: "transport_error"}
		}
	}
	families := map[string]bool{}
	for _, attempt := range attempts {
		report := parseProbeJSONText(attempt.ResponseText)
		if report == nil {
			return probeClassification{Result: "indeterminate"}
		}
		if family := strings.ToLower(strings.TrimSpace(anyToString(report["self_reported_family"]))); family != "" {
			families[family] = true
		}
	}
	if len(families) > 1 {
		return probeClassification{Result: "warning", ResultType: "probe_identity_consistency_warning", Confidence: "medium"}
	}
	return probeClassification{Result: "pass", Confidence: "low"}
}

func validateKnowledgeCutoffAnswer(id string, text string) bool {
	lower := strings.ToLower(text)
	switch id {
	case "anchor_1":
		return strings.Contains(lower, "donald trump") || strings.Contains(lower, "特朗普")
	case "anchor_2":
		return strings.Contains(lower, "2025")
	default:
		return true
	}
}

func classifyKnowledgeCutoffProbeResult(results map[string]probeAttempt) probeClassification {
	for _, attempt := range results {
		if attempt.RequestError != nil || (attempt.ResponseStatus != nil && *attempt.ResponseStatus >= 500) {
			return probeClassification{Result: "transport_error"}
		}
	}
	selfReport := parseProbeJSONText(results["self_cutoff"].ResponseText)
	claimsCutoff := strings.ToLower(strings.TrimSpace(anyToString(selfReport["claims_cutoff"])))
	claimsEarlyCutoff := claimsCutoff != "" && claimsCutoff != "unknown" && claimsCutoff < "2025-01-01"
	anchorFailures := 0
	for id, attempt := range results {
		if id == "self_cutoff" {
			continue
		}
		if !validateKnowledgeCutoffAnswer(id, attempt.ResponseText) {
			anchorFailures++
		}
	}
	if claimsEarlyCutoff && anchorFailures >= 1 {
		return probeClassification{Result: "warning", ResultType: "probe_knowledge_cutoff_warning", Confidence: "low"}
	}
	if !claimsEarlyCutoff && anchorFailures == 0 {
		return probeClassification{Result: "pass", Confidence: "low"}
	}
	return probeClassification{Result: "indeterminate"}
}

func buildAggregateProbeSample(probeType string, targetModel string, targetFamily string, endpointPath string, classified probeClassification, attempts []probeAttempt, probeLogs []string) probeSample {
	durationMS := 0
	httpStatuses := []int{}
	var httpStatus *int
	errorExcerpt := ""
	context := createRequestModelContext(targetModel, targetModel)
	for _, attempt := range attempts {
		durationMS += attempt.DurationMS
		if attempt.ResponseStatus != nil {
			status := *attempt.ResponseStatus
			httpStatuses = append(httpStatuses, status)
			httpStatus = &status
		}
		if errorExcerpt == "" {
			if attempt.RequestError != nil {
				errorExcerpt = attempt.RequestError.Error()
			} else {
				errorExcerpt = attempt.ResponseBodyExcerpt
			}
		}
		if attempt.ModelContext != nil {
			for model := range attempt.ModelContext.ObservedModels {
				context.ObservedModels[model] = true
			}
			for fingerprint := range attempt.ModelContext.ObservedFingerprints {
				context.ObservedFingerprints[fingerprint] = true
			}
			context.UpstreamModel = firstNonEmpty(attempt.ModelContext.UpstreamModel, context.UpstreamModel)
			context.StreamModel = firstNonEmpty(attempt.ModelContext.StreamModel, context.StreamModel)
			context.FinalResponseModel = firstNonEmpty(attempt.ModelContext.FinalResponseModel, context.FinalResponseModel)
		}
	}
	probeLogs = append(probeLogs, fmt.Sprintf("[probe] finish type=%s family=%s status=%v result=%s result_type=%s confidence=%s", probeType, targetFamily, httpStatus, classified.Result, firstNonEmpty(classified.ResultType, "-"), firstNonEmpty(classified.Confidence, "-")))
	return probeSample{
		TS:                   time.Now().Format(time.RFC3339),
		ProbeType:            probeType,
		TargetModel:          targetModel,
		TargetFamily:         targetFamily,
		EndpointPath:         endpointPath,
		Result:               classified.Result,
		ResultType:           classified.ResultType,
		Confidence:           classified.Confidence,
		HTTPStatus:           httpStatus,
		DurationMS:           durationMS,
		ErrorExcerpt:         errorExcerpt,
		UpstreamModel:        context.UpstreamModel,
		StreamModel:          context.StreamModel,
		FinalResponseModel:   context.FinalResponseModel,
		ObservedModels:       sortedStringSet(context.ObservedModels),
		ObservedFingerprints: sortedStringSet(context.ObservedFingerprints),
		AttemptCount:         len(attempts),
		HTTPStatuses:         httpStatuses,
		EvidenceLogs:         collectProbeEvidenceLogs(probeLogs, probeType),
	}
}

func runImageInputProbe(runtime *appRuntime, targetModel string, targetFamily string) probeSample {
	endpointPath := runtime.ActiveProbe.LastSuccessfulEndpoint
	if endpointPath == "" {
		endpointPath = chooseProbeEndpoint(runtime.Config.ActiveProbe)
	}
	payload := map[string]any{
		"model": targetModel,
		"input": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "__crg_image_input_probe__ 请只回答图片里的大写字母。"},
				map[string]any{"type": "input_image", "image_url": probeImageDataURL},
			},
		}},
	}
	return executeProbeRequest(runtime, "image_input", endpointPath, payload, targetModel, targetFamily, classifyImageProbeResult)
}

func runLongContextProbe(runtime *appRuntime, targetModel string, targetFamily string) probeSample {
	endpointPath := runtime.ActiveProbe.LastSuccessfulEndpoint
	if endpointPath == "" {
		endpointPath = chooseProbeEndpoint(runtime.Config.ActiveProbe)
	}
	targetInputTokens := activeProbeMapInt(runtime.Config.ActiveProbe.LongContext, "target_input_tokens", 460000)
	probeLogs := []string{fmt.Sprintf("[probe] start type=long_context family=%s endpoint=%s target_input_tokens=%d budget_source=response_usage", targetFamily, endpointPath, targetInputTokens)}
	finalizeSample := func(classified probeClassification, attempt probeAttempt, observedInputTokens *int, estimatedInputTokens *int, baselineInputTokens *int, seedInputTokens *int, unitCount *int, calibrationRounds int) probeSample {
		errorExcerpt := attempt.ResponseBodyExcerpt
		if attempt.RequestError != nil {
			errorExcerpt = attempt.RequestError.Error()
		}
		budgetDetail := buildLongContextBudgetDetail(targetInputTokens, firstIntPointer(observedInputTokens, attempt.InputTokens), estimatedInputTokens, baselineInputTokens, seedInputTokens, unitCount, calibrationRounds)
		errorExcerpt = combineProbeDetail(errorExcerpt, budgetDetail)
		statusText := "-"
		if attempt.ResponseStatus != nil {
			statusText = fmt.Sprintf("%d", *attempt.ResponseStatus)
		}
		probeLogs = append(probeLogs, fmt.Sprintf("[probe] finish type=long_context family=%s status=%s result=%s result_type=%s confidence=%s", targetFamily, statusText, classified.Result, firstNonEmpty(classified.ResultType, "-"), firstNonEmpty(classified.Confidence, "-")))
		if errorExcerpt != "" {
			probeLogs = append(probeLogs, fmt.Sprintf("[probe] evidence type=long_context family=%s detail=%s", targetFamily, errorExcerpt))
		}
		sample := probeSample{
			TS:                   time.Now().Format(time.RFC3339),
			ProbeType:            "long_context",
			TargetModel:          targetModel,
			TargetFamily:         targetFamily,
			EndpointPath:         endpointPath,
			Result:               classified.Result,
			ResultType:           classified.ResultType,
			Confidence:           classified.Confidence,
			HTTPStatus:           attempt.ResponseStatus,
			DurationMS:           attempt.DurationMS,
			ErrorExcerpt:         errorExcerpt,
			UpstreamModel:        attempt.ModelContext.UpstreamModel,
			StreamModel:          attempt.ModelContext.StreamModel,
			FinalResponseModel:   attempt.ModelContext.FinalResponseModel,
			ObservedModels:       sortedStringSet(attempt.ModelContext.ObservedModels),
			ObservedFingerprints: sortedStringSet(attempt.ModelContext.ObservedFingerprints),
			RequestedInputTokens: &targetInputTokens,
			ObservedInputTokens:  firstIntPointer(observedInputTokens, attempt.InputTokens),
			EstimatedInputTokens: estimatedInputTokens,
			TokenBudgetSource:    "response_usage",
			CalibrationRounds:    &calibrationRounds,
			EvidenceLogs:         collectProbeEvidenceLogs(probeLogs, "long_context"),
		}
		return sample
	}

	runBudgetAttempt := func(unitCount int, phase string) probeAttempt {
		return executeProbeAttempt(runtime, endpointPath, buildLongContextProbePayload(targetModel, unitCount, phase), targetModel)
	}
	baselineAttempt := runBudgetAttempt(0, "baseline")
	baselineClassified := classifyLongContextProbeResult(baselineAttempt.ResponseStatus, baselineAttempt.ParsedBody, baselineAttempt.RequestError)
	if baselineClassified.Result != "pass" {
		zero := 0
		return finalizeSample(baselineClassified, baselineAttempt, nil, nil, nil, nil, &zero, 1)
	}
	if baselineAttempt.InputTokens == nil {
		zero := 0
		return finalizeSample(probeClassification{Result: "indeterminate"}, baselineAttempt, nil, nil, nil, nil, &zero, 1)
	}

	seedUnitCount := max(1024, min(longContextProbeSeedUnitCount, targetInputTokens))
	seedAttempt := runBudgetAttempt(seedUnitCount, "seed")
	seedClassified := classifyLongContextProbeResult(seedAttempt.ResponseStatus, seedAttempt.ParsedBody, seedAttempt.RequestError)
	if seedClassified.Result != "pass" {
		return finalizeSample(seedClassified, seedAttempt, nil, nil, baselineAttempt.InputTokens, nil, &seedUnitCount, 2)
	}
	if seedAttempt.InputTokens == nil || *seedAttempt.InputTokens <= *baselineAttempt.InputTokens {
		return finalizeSample(probeClassification{Result: "indeterminate"}, seedAttempt, nil, nil, baselineAttempt.InputTokens, nil, &seedUnitCount, 2)
	}

	unitCountPtr := estimateLongContextUnitCount(*baselineAttempt.InputTokens, *seedAttempt.InputTokens, seedUnitCount, targetInputTokens)
	if unitCountPtr == nil || *unitCountPtr <= 0 {
		return finalizeSample(probeClassification{Result: "indeterminate"}, seedAttempt, nil, nil, baselineAttempt.InputTokens, seedAttempt.InputTokens, &seedUnitCount, 2)
	}

	unitCount := *unitCountPtr
	finalAttempt := seedAttempt
	var estimatedInputTokens *int
	calibrationRounds := 2
	for attemptIndex := 0; attemptIndex < longContextProbeMaxBudgetAttempt; attemptIndex++ {
		estimated := *baselineAttempt.InputTokens + int(float64(max(0, *seedAttempt.InputTokens-*baselineAttempt.InputTokens))*(float64(unitCount)/float64(seedUnitCount)))
		estimatedInputTokens = &estimated
		probeLogs = append(probeLogs, fmt.Sprintf("[probe] budget type=long_context family=%s target_input_tokens=%d baseline_input_tokens=%d seed_input_tokens=%d unit_count=%d estimated_input_tokens=%d", targetFamily, targetInputTokens, *baselineAttempt.InputTokens, *seedAttempt.InputTokens, unitCount, estimated))
		phase := "budget"
		if attemptIndex > 0 {
			phase = fmt.Sprintf("budget_refine_%d", attemptIndex)
		}
		finalAttempt = runBudgetAttempt(unitCount, phase)
		calibrationRounds++
		finalClassified := classifyLongContextProbeResult(finalAttempt.ResponseStatus, finalAttempt.ParsedBody, finalAttempt.RequestError)
		if finalClassified.Result != "pass" {
			return finalizeSample(finalClassified, finalAttempt, nil, estimatedInputTokens, baselineAttempt.InputTokens, seedAttempt.InputTokens, &unitCount, calibrationRounds)
		}
		if finalAttempt.InputTokens != nil && *finalAttempt.InputTokens >= targetInputTokens-longContextProbeTokenTolerance {
			return finalizeSample(finalClassified, finalAttempt, finalAttempt.InputTokens, estimatedInputTokens, baselineAttempt.InputTokens, seedAttempt.InputTokens, &unitCount, calibrationRounds)
		}
		if finalAttempt.InputTokens == nil {
			break
		}
		remainingTokens := targetInputTokens - *finalAttempt.InputTokens
		if remainingTokens <= longContextProbeTokenTolerance {
			return finalizeSample(finalClassified, finalAttempt, finalAttempt.InputTokens, estimatedInputTokens, baselineAttempt.InputTokens, seedAttempt.InputTokens, &unitCount, calibrationRounds)
		}
		tokensPerUnit := float64(*seedAttempt.InputTokens-*baselineAttempt.InputTokens) / float64(seedUnitCount)
		if tokensPerUnit <= 0 {
			break
		}
		nextUnitCount := unitCount + max(1, intCeil(float64(remainingTokens)/tokensPerUnit))
		if nextUnitCount <= unitCount {
			break
		}
		unitCount = nextUnitCount
	}
	return finalizeSample(probeClassification{Result: "indeterminate"}, finalAttempt, finalAttempt.InputTokens, estimatedInputTokens, baselineAttempt.InputTokens, seedAttempt.InputTokens, &unitCount, calibrationRounds)
}

func runResponseStructureProbe(runtime *appRuntime, targetModel string, targetFamily string) probeSample {
	endpointPath := runtime.ActiveProbe.LastSuccessfulEndpoint
	if endpointPath == "" {
		endpointPath = chooseProbeEndpoint(runtime.Config.ActiveProbe)
	}
	repeatCount := max(1, activeProbeMapInt(runtime.Config.ActiveProbe.ResponseStructure, "repeat_count", 2))
	probeLogs := []string{fmt.Sprintf("[probe] start type=response_structure family=%s endpoint=%s", targetFamily, endpointPath)}
	attempts := make([]probeAttempt, 0, repeatCount)
	for index := 0; index < repeatCount; index++ {
		payload := map[string]any{
			"model": targetModel,
			"input": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": `__crg_response_structure_probe__ 请只输出 JSON，不要额外文本。把 a=1,b=2,c=3 转成 {"items":[{"key":"a","value":1},{"key":"b","value":2},{"key":"c","value":3}]}`,
				}},
			}},
		}
		attempts = append(attempts, executeProbeAttempt(runtime, endpointPath, payload, targetModel))
	}
	return buildAggregateProbeSample("response_structure", targetModel, targetFamily, endpointPath, classifyResponseStructureProbeResult(attempts), attempts, probeLogs)
}

func runIdentityConsistencyProbe(runtime *appRuntime, targetModel string, targetFamily string) probeSample {
	endpointPath := runtime.ActiveProbe.LastSuccessfulEndpoint
	if endpointPath == "" {
		endpointPath = chooseProbeEndpoint(runtime.Config.ActiveProbe)
	}
	repeatCount := max(1, activeProbeMapInt(runtime.Config.ActiveProbe.IdentityConsistency, "repeat_count", 2))
	probeLogs := []string{fmt.Sprintf("[probe] start type=identity_consistency family=%s endpoint=%s", targetFamily, endpointPath)}
	attempts := make([]probeAttempt, 0, repeatCount)
	for index := 0; index < repeatCount; index++ {
		payload := map[string]any{
			"model": targetModel,
			"input": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": `__crg_identity_probe__ 请只输出 JSON：{"self_reported_model":"...","self_reported_family":"...","claims_image_input":true,"claims_cutoff":"YYYY-MM-DD or unknown"}`,
				}},
			}},
		}
		attempts = append(attempts, executeProbeAttempt(runtime, endpointPath, payload, targetModel))
	}
	return buildAggregateProbeSample("identity_consistency", targetModel, targetFamily, endpointPath, classifyIdentityConsistencyProbeResult(attempts), attempts, probeLogs)
}

func runKnowledgeCutoffProbe(runtime *appRuntime, targetModel string, targetFamily string) probeSample {
	endpointPath := runtime.ActiveProbe.LastSuccessfulEndpoint
	if endpointPath == "" {
		endpointPath = chooseProbeEndpoint(runtime.Config.ActiveProbe)
	}
	maxQuestions := max(1, activeProbeMapInt(runtime.Config.ActiveProbe.KnowledgeCutoff, "max_questions", 3))
	questions := []struct {
		ID     string
		Prompt string
	}{
		{ID: "self_cutoff", Prompt: `__crg_knowledge_cutoff_probe__:self_cutoff 请只输出 JSON：{"claims_cutoff":"YYYY-MM-DD or unknown"}`},
		{ID: "anchor_1", Prompt: "__crg_knowledge_cutoff_probe__:anchor_1 2025-01-20 就任的美国总统是谁？只回答人名。"},
		{ID: "anchor_2", Prompt: "__crg_knowledge_cutoff_probe__:anchor_2 唐纳德·特朗普再次就任美国总统的年份是几？只回答四位数字年份。"},
	}
	if maxQuestions < len(questions) {
		questions = questions[:maxQuestions]
	}
	probeLogs := []string{fmt.Sprintf("[probe] start type=knowledge_cutoff family=%s endpoint=%s", targetFamily, endpointPath)}
	results := map[string]probeAttempt{}
	attempts := make([]probeAttempt, 0, len(questions))
	for _, question := range questions {
		payload := map[string]any{
			"model": targetModel,
			"input": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": question.Prompt}},
			}},
		}
		attempt := executeProbeAttempt(runtime, endpointPath, payload, targetModel)
		results[question.ID] = attempt
		attempts = append(attempts, attempt)
	}
	return buildAggregateProbeSample("knowledge_cutoff", targetModel, targetFamily, endpointPath, classifyKnowledgeCutoffProbeResult(results), attempts, probeLogs)
}

func startActiveProbeRun(runtime *appRuntime, config activeProbeConfig, manual bool) (map[string]any, int) {
	monitor := runtime.ActiveProbe
	if monitor == nil {
		monitor = newActiveProbeMonitor()
		runtime.ActiveProbe = monitor
	}
	monitor.mu.Lock()
	if monitor.Running {
		monitor.mu.Unlock()
		return map[string]any{
			"ok":           false,
			"message":      "主动探针正在运行中，请稍后再试",
			"active_probe": buildActiveProbeSnapshot(runtime),
		}, http.StatusConflict
	}
	monitor.Running = true
	monitor.Enabled = config.Enabled
	monitor.TotalRuns++
	monitor.LastStartedAt = time.Now().Format(time.RFC3339)
	monitor.mu.Unlock()

	go func() {
		localModel := getLocalConfigModel(runtime)
		targets := resolveActiveProbeTargets(gatewayConfig{ActiveProbe: config}, localModel)
		if len(targets) == 0 {
			monitor.mu.Lock()
			monitor.LastTargetModel = localModel
			monitor.LastTargetFamily = normalizeModelFamily(localModel)
			monitor.SkippedRuns++
			monitor.Running = false
			monitor.LastFinishedAt = time.Now().Format(time.RFC3339)
			monitor.mu.Unlock()
			if runtime.Logger != nil {
				runtime.Logger(fmt.Sprintf("[probe] skip reason=untracked_family family=%s", normalizeModelFamily(localModel)))
			}
			return
		}
		samples := []probeSample{}
		for _, target := range targets {
			monitor.mu.Lock()
			monitor.LastTargetModel = target.Model
			monitor.LastTargetFamily = target.Family
			monitor.mu.Unlock()
			if activeProbeMapEnabled(config.LongContext, true) {
				samples = append(samples, runLongContextProbe(runtime, target.Model, target.Family))
			}
			if activeProbeMapEnabled(config.ImageInput, true) {
				samples = append(samples, runImageInputProbe(runtime, target.Model, target.Family))
			}
			if activeProbeMapEnabled(config.ResponseStructure, false) {
				samples = append(samples, runResponseStructureProbe(runtime, target.Model, target.Family))
			}
			if activeProbeMapEnabled(config.IdentityConsistency, false) {
				samples = append(samples, runIdentityConsistencyProbe(runtime, target.Model, target.Family))
			}
			if activeProbeMapEnabled(config.KnowledgeCutoff, false) {
				samples = append(samples, runKnowledgeCutoffProbe(runtime, target.Model, target.Family))
			}
		}
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		monitor.Running = false
		monitor.LastFinishedAt = time.Now().Format(time.RFC3339)
		for _, sample := range samples {
			pushProbeSample(monitor, sample)
			applyProbeResultCounters(monitor, sample)
		}
	}()

	return map[string]any{
		"ok":           true,
		"message":      "主动探针已开始，请稍后查看状态",
		"active_probe": buildActiveProbeSnapshot(runtime),
	}, http.StatusAccepted
}

func clearActiveProbeSchedule(runtime *appRuntime) {
	if runtime == nil {
		return
	}
	if runtime.ProbeStartupTimer != nil {
		runtime.ProbeStartupTimer.Stop()
		runtime.ProbeStartupTimer = nil
	}
	if runtime.ProbeTimer != nil {
		runtime.ProbeTimer.Stop()
		runtime.ProbeTimer = nil
	}
}

func scheduleActiveProbes(runtime *appRuntime) {
	if runtime == nil {
		return
	}
	clearActiveProbeSchedule(runtime)
	if !runtime.Config.ActiveProbe.Enabled {
		return
	}
	startupDelay := time.Duration(runtime.Config.ActiveProbe.StartupDelayMS) * time.Millisecond
	if startupDelay < 0 {
		startupDelay = 0
	}
	runtime.ProbeStartupTimer = time.AfterFunc(startupDelay, func() {
		_, _ = startActiveProbeRun(runtime, runtime.Config.ActiveProbe, false)
		interval := time.Duration(runtime.Config.ActiveProbe.IntervalMS) * time.Millisecond
		if interval <= 0 {
			return
		}
		runtime.ProbeTimer = time.NewTicker(interval)
		go func() {
			for range runtime.ProbeTimer.C {
				_, _ = startActiveProbeRun(runtime, runtime.Config.ActiveProbe, false)
			}
		}()
	})
}
