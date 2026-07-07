package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const upstreamCapacityErrorMessage = "Selected model is at capacity. Please try a different model."

type gatewayError struct {
	StatusCode int
	ErrorType  string
	Code       string
	Message    string
}

func (err *gatewayError) Error() string {
	return err.Message
}

type requestTracking struct {
	ID                     string
	Pathname               string
	Method                 string
	RequestKind            string
	RequestJSON            map[string]any
	RequestSummary         map[string]any
	GuardRetryRemaining    int
	ContinuationBaseJSON   map[string]any
	StripEncryptedResponse bool
	RequestReasoningEffort string
	RequestModel           string
}

type structureAccumulator struct {
	EventTypeCounts        map[string]int
	ResponseItemTypeCounts map[string]int
	HasCommentary          bool
	HasFinalAnswer         bool
	HasToolCall            bool
	HasOutputText          bool
	HasReasoningItem       bool
}

type interceptRuleMatch struct {
	Mode         string
	Matched      bool
	ReasonForLog string
	Reasoning    *int
	ExemptReason string
}

type handlerResult struct {
	ContinuationRecovery bool
	GuardRetry           bool
	RequestJSON          map[string]any
	RequestBody          []byte
	Handled              bool
}

func buildUpstreamURL(baseURL string, requestURL *url.URL) (string, error) {
	upstream, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimSuffix(upstream.Path, "/")
	incomingPath := requestURL.Path
	finalPath := incomingPath
	if basePath != "" && basePath != "/" {
		switch {
		case strings.HasPrefix(incomingPath, basePath+"/") || incomingPath == basePath:
			finalPath = incomingPath
		case strings.HasSuffix(basePath, "/v1") && strings.HasPrefix(incomingPath, "/v1/"):
			finalPath = basePath + incomingPath[3:]
		default:
			finalPath = basePath + incomingPath
		}
	}
	upstream.Path = finalPath
	upstream.RawQuery = requestURL.RawQuery
	return upstream.String(), nil
}

func matchPath(config gatewayConfig, pathname string) bool {
	for _, endpoint := range config.Endpoints {
		if normalizePath(endpoint) == normalizePath(pathname) {
			return true
		}
	}
	return false
}

func readRequestBody(request *http.Request, limit int64) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	defer request.Body.Close()

	var buffer bytes.Buffer
	reader := bufio.NewReader(request.Body)
	for {
		chunk := make([]byte, 32*1024)
		n, err := reader.Read(chunk)
		if n > 0 {
			if int64(buffer.Len()+n) > limit {
				return nil, &gatewayError{
					StatusCode: http.StatusRequestEntityTooLarge,
					ErrorType:  "gateway_rejection",
					Code:       "request_body_limit_exceeded",
					Message:    "request_body_limit_exceeded",
				}
			}
			buffer.Write(chunk[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return buffer.Bytes(), nil
}

func isJSONContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "application/json")
}

func isSSEContentType(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func extractReasoningTokens(payload map[string]any) *int {
	pointers := [][]string{
		{"usage", "output_tokens_details", "reasoning_tokens"},
		{"usage", "reasoning_tokens"},
		{"reasoning_tokens"},
	}
	for _, pointer := range pointers {
		if value, ok := nestedInt(payload, pointer...); ok {
			return &value
		}
	}
	return nil
}

func nestedInt(payload map[string]any, path ...string) (int, bool) {
	var current any = payload
	for _, key := range path {
		typed, ok := current.(map[string]any)
		if !ok {
			return 0, false
		}
		current, ok = typed[key]
		if !ok {
			return 0, false
		}
	}
	switch value := current.(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case json.Number:
		parsed, err := value.Int64()
		return int(parsed), err == nil
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(value))
		return parsed, err == nil
	default:
		return 0, false
	}
}

func detectRequestKind(headers http.Header, requestJSON map[string]any) string {
	signals := strings.ToLower(strings.Join([]string{
		headers.Get("x-codex-request-kind"),
		headers.Get("x-codex-purpose"),
		headers.Get("x-codex-turn-metadata"),
	}, " "))
	if includesAnyContextCompactionMarker(signals) {
		return requestKindContextCompaction
	}
	metadataParts := []string{
		anyToString(requestJSON["metadata"]),
		anyToString(requestJSON["codex_request_kind"]),
		anyToString(requestJSON["request_kind"]),
		anyToString(requestJSON["purpose"]),
	}
	if includesAnyContextCompactionMarker(strings.ToLower(strings.Join(metadataParts, " "))) {
		return requestKindContextCompaction
	}
	return requestKindNormal
}

func includesAnyContextCompactionMarker(value string) bool {
	normalized := strings.TrimSpace(strings.ToLower(value))
	if normalized == "" {
		return false
	}
	for _, marker := range contextCompactionMarkers {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func extractRequestReasoningEffort(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	value := strings.TrimSpace(strings.ToLower(anyToString(reasoning["effort"])))
	switch value {
	case "low", "medium", "high", "xhigh":
		return value
	default:
		return ""
	}
}

func reasoningMatched(config gatewayConfig, reasoning *int) bool {
	if reasoning == nil {
		return false
	}
	switch config.ReasoningMatchMode {
	case defaultReasoningMatchMode:
		value := *reasoning
		return value >= 516 && (value+2)%518 == 0
	default:
		for _, candidate := range config.ReasoningEquals {
			if candidate == *reasoning {
				return true
			}
		}
		return false
	}
}

func isContextCompactionExemptReasoning(reasoning *int) bool {
	return reasoning != nil && *reasoning == 0
}

func createStructureAccumulator() *structureAccumulator {
	return &structureAccumulator{
		EventTypeCounts:        map[string]int{},
		ResponseItemTypeCounts: map[string]int{},
	}
}

func markVisibleContent(structure *structureAccumulator) {
	structure.HasFinalAnswer = true
	structure.HasOutputText = true
}

func inspectContentEntry(entry map[string]any, structure *structureAccumulator) {
	contentType := strings.ToLower(strings.TrimSpace(anyToString(entry["type"])))
	if strings.Contains(contentType, "commentary") {
		structure.HasCommentary = true
	}
	if strings.Contains(contentType, "tool_call") || strings.Contains(contentType, "function_call") {
		structure.HasToolCall = true
	}
	for _, key := range []string{"text", "output_text", "content"} {
		if value := strings.TrimSpace(anyToString(entry[key])); value != "" {
			markVisibleContent(structure)
		}
	}
}

func inspectOutputItem(item map[string]any, structure *structureAccumulator) {
	itemType := strings.ToLower(strings.TrimSpace(anyToString(item["type"])))
	if itemType == "" {
		itemType = "unknown"
	}
	structure.ResponseItemTypeCounts[itemType]++
	if strings.Contains(itemType, "reasoning") {
		structure.HasReasoningItem = true
	}
	if strings.Contains(itemType, "commentary") {
		structure.HasCommentary = true
	}
	if strings.Contains(itemType, "tool_call") || strings.Contains(itemType, "function_call") || strings.Contains(itemType, "tool") {
		structure.HasToolCall = true
	}
	for _, key := range []string{"text", "output_text"} {
		if value := strings.TrimSpace(anyToString(item[key])); value != "" {
			markVisibleContent(structure)
		}
	}
	if contentItems, ok := item["content"].([]any); ok {
		for _, raw := range contentItems {
			if entry, ok := raw.(map[string]any); ok {
				inspectContentEntry(entry, structure)
			}
		}
	}
}

func applyStructureSignalsFromPayload(payload map[string]any, structure *structureAccumulator, fromStream bool) {
	eventType := strings.ToLower(strings.TrimSpace(anyToString(payload["type"])))
	if fromStream && eventType != "" {
		structure.EventTypeCounts[eventType]++
	}
	if strings.Contains(eventType, "commentary") {
		structure.HasCommentary = true
	}
	if strings.Contains(eventType, "tool_call") || strings.Contains(eventType, "function_call") {
		structure.HasToolCall = true
	}
	if strings.Contains(eventType, "output_text.delta") || strings.Contains(eventType, "message.delta") || strings.Contains(eventType, "content.delta") {
		if strings.TrimSpace(anyToString(payload["delta"])) != "" || strings.TrimSpace(anyToString(payload["text"])) != "" || strings.TrimSpace(anyToString(payload["content"])) != "" {
			markVisibleContent(structure)
		}
	}
	if choices, ok := payload["choices"].([]any); ok {
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			if delta, ok := choice["delta"].(map[string]any); ok && strings.TrimSpace(anyToString(delta["content"])) != "" {
				markVisibleContent(structure)
			}
			if message, ok := choice["message"].(map[string]any); ok && strings.TrimSpace(anyToString(message["content"])) != "" {
				markVisibleContent(structure)
			}
		}
	}
	if strings.TrimSpace(anyToString(payload["output_text"])) != "" {
		markVisibleContent(structure)
	}
	if item, ok := payload["item"].(map[string]any); ok {
		inspectOutputItem(item, structure)
	}
	for _, key := range []string{"output", "response"} {
		switch value := payload[key].(type) {
		case []any:
			for _, raw := range value {
				if item, ok := raw.(map[string]any); ok {
					inspectOutputItem(item, structure)
				}
			}
		case map[string]any:
			if output, ok := value["output"].([]any); ok {
				for _, raw := range output {
					if item, ok := raw.(map[string]any); ok {
						inspectOutputItem(item, structure)
					}
				}
			}
		}
	}
}

func isFinalAnswerOnlyStructure(structure *structureAccumulator) bool {
	return structure.HasFinalAnswer && !structure.HasCommentary && !structure.HasToolCall && !structure.HasReasoningItem
}

func buildInterceptRuleMatch(config gatewayConfig, reasoning *int, tracking *requestTracking, structure *structureAccumulator) interceptRuleMatch {
	mode := normalizeInterceptRuleMode(config.InterceptRuleMode, defaultInterceptRuleMode)
	if tracking.RequestKind == requestKindContextCompaction && isContextCompactionExemptReasoning(reasoning) {
		return interceptRuleMatch{
			Mode:         mode,
			Matched:      false,
			ReasonForLog: fmt.Sprintf("request_kind=%s intercept_exempt_reason=%s reasoning_tokens=%s", requestKindContextCompaction, requestKindContextCompaction, reasoningString(reasoning)),
			Reasoning:    reasoning,
			ExemptReason: requestKindContextCompaction,
		}
	}
	if mode == finalOnlyRuleMode {
		finalOnly := isFinalAnswerOnlyStructure(structure)
		effort := strings.TrimSpace(strings.ToLower(tracking.RequestReasoningEffort))
		reasoningAllowed := reasoning == nil || *reasoning != 0
		return interceptRuleMatch{
			Mode:         mode,
			Matched:      finalOnly && reasoningAllowed && finalOnlyInterceptEfforts[effort],
			ReasonForLog: fmt.Sprintf("final_answer_only=%t effort=%s reasoning_tokens=%s zero_reasoning_excluded=%t", finalOnly, effortOrUnknown(effort), reasoningString(reasoning), reasoning != nil && *reasoning == 0),
			Reasoning:    reasoning,
		}
	}
	return interceptRuleMatch{
		Mode:         mode,
		Matched:      reasoningMatched(config, reasoning),
		ReasonForLog: fmt.Sprintf("reasoning_tokens=%s", reasoningString(reasoning)),
		Reasoning:    reasoning,
	}
}

func reasoningString(value *int) string {
	if value == nil {
		return "null"
	}
	return strconv.Itoa(*value)
}

func effortOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func isUpstreamCapacityErrorResponse(response *http.Response, parsed map[string]any, body []byte) bool {
	if response == nil || response.StatusCode < 400 {
		return false
	}
	text := strings.ToLower(upstreamCapacityErrorMessage)
	return anyContainsText(parsed, func(source string) bool {
		return strings.Contains(source, text) || (strings.Contains(source, "selected model is at capacity") && strings.Contains(source, "try a different model"))
	}) || strings.Contains(strings.ToLower(string(body)), text)
}

func buildBlockedBody(pathname string, reasoning *int, statusCode int) []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message":          fmt.Sprintf("codex retry gateway blocked suspicious reasoning response on %s", pathname),
			"type":             "codex_retry_gateway",
			"code":             "reasoning_guard_triggered",
			"reasoning_tokens": reasoningValue(reasoning),
			"status_code":      statusCode,
		},
	})
	return body
}

func reasoningValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func normalizeResponsesInputForContinuation(input any) []any {
	var items []any
	switch typed := input.(type) {
	case nil:
		return []any{}
	case []any:
		items = typed
	default:
		items = []any{typed}
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		sanitized := sanitizeResponsesInputItemForContinuationReplay(item)
		if sanitized != nil {
			result = append(result, sanitized)
		}
	}
	return result
}

func sanitizeResponsesInputItemForContinuationReplay(item any) any {
	switch typed := item.(type) {
	case string:
		return map[string]any{"type": "message", "role": "user", "content": typed}
	case map[string]any:
		itemType := strings.ToLower(strings.TrimSpace(anyToString(typed["type"])))
		if itemType == "reasoning" {
			return nil
		}
		return stripEncryptedContent(typed)
	default:
		return item
	}
}

func removeContinuationEncryptedInclude(include any) any {
	items, ok := include.([]any)
	if !ok {
		return include
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		value := strings.TrimSpace(anyToString(item))
		if value == "" || value == "reasoning.encrypted_content" {
			continue
		}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func buildContinuationMarkerItem(config gatewayConfig) map[string]any {
	return map[string]any{
		"type":  "message",
		"role":  "assistant",
		"phase": "commentary",
		"content": []any{
			map[string]any{
				"type": "output_text",
				"text": normalizeContinuationMarkerText(config.ContinuationMarkerText, defaultContinuationMarkerText),
			},
		},
	}
}

func buildContinuationRecoveryRequestBody(config gatewayConfig, baseRequestJSON map[string]any) (map[string]any, []byte, error) {
	nextBody := cloneJSONMap(baseRequestJSON)
	if nextBody == nil {
		nextBody = map[string]any{}
	}
	delete(nextBody, "previous_response_id")
	if nextInclude := removeContinuationEncryptedInclude(nextBody["include"]); nextInclude == nil {
		delete(nextBody, "include")
	} else {
		nextBody["include"] = nextInclude
	}
	nextBody["stream"] = true
	nextBody["input"] = append(normalizeResponsesInputForContinuation(baseRequestJSON["input"]), buildContinuationMarkerItem(config))
	body, err := json.Marshal(nextBody)
	return nextBody, body, err
}

func stripEncryptedContentFromSSEBody(body []byte) []byte {
	blocks := strings.Split(string(body), "\n\n")
	result := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			result = append(result, block)
			continue
		}
		lines := strings.Split(block, "\n")
		outputLines := make([]string, 0, len(lines))
		for _, line := range lines {
			if !strings.HasPrefix(line, "data:") {
				if strings.Contains(strings.ToLower(line), "encrypted_content") {
					outputLines = append(outputLines, "event: gateway.redacted")
				} else {
					outputLines = append(outputLines, line)
				}
				continue
			}
			payloadText := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payloadText == "[DONE]" {
				outputLines = append(outputLines, line)
				continue
			}
			if parsed := parseJSON([]byte(payloadText)); parsed != nil {
				redacted, _ := json.Marshal(stripEncryptedContent(parsed))
				outputLines = append(outputLines, "data: "+string(redacted))
				continue
			}
			if strings.Contains(strings.ToLower(payloadText), "encrypted_content") {
				redacted, _ := json.Marshal(map[string]any{"type": "gateway.redacted", "redacted": true})
				outputLines = append(outputLines, "data: "+string(redacted))
				continue
			}
			outputLines = append(outputLines, line)
		}
		result = append(result, strings.Join(outputLines, "\n"))
	}
	return []byte(strings.Join(result, "\n\n"))
}

type streamInspection struct {
	Reasoning  *int
	Structure  *structureAccumulator
	Payloads   []map[string]any
	Matched    interceptRuleMatch
}

func inspectSSEBody(config gatewayConfig, body []byte, tracking *requestTracking) streamInspection {
	structure := createStructureAccumulator()
	var observedReasoning *int
	payloads := []map[string]any{}
	blocks := strings.Split(string(body), "\n\n")
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		lines := strings.Split(block, "\n")
		dataLines := make([]string, 0, len(lines))
		for _, line := range lines {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if len(dataLines) == 0 {
			continue
		}
		payloadText := strings.Join(dataLines, "\n")
		if payloadText == "[DONE]" {
			continue
		}
		payload := parseJSON([]byte(payloadText))
		if payload == nil {
			continue
		}
		payloads = append(payloads, payload)
		applyStructureSignalsFromPayload(payload, structure, true)
		if value := extractReasoningTokens(payload); value != nil {
			observedReasoning = value
		}
	}
	return streamInspection{
		Reasoning: observedReasoning,
		Structure: structure,
		Payloads:  payloads,
		Matched:   buildInterceptRuleMatch(config, observedReasoning, tracking, structure),
	}
}

func proxyRequest(runtime *appRuntime, writer http.ResponseWriter, request *http.Request) {
	incomingURL := &url.URL{Path: request.URL.Path, RawQuery: request.URL.RawQuery}
	pathname := normalizePath(request.URL.Path)
	tracking := &requestTracking{
		ID:       runtime.nextRequestID(),
		Pathname: pathname,
		Method:   request.Method,
	}
	runtime.Monitor.mu.Lock()
	runtime.Monitor.TotalProxyRequestCount++
	runtime.Monitor.ActiveProxyRequestCount++
	runtime.Monitor.ActiveProxyPathCounts[pathname]++
	runtime.Monitor.mu.Unlock()
	defer func() {
		runtime.Monitor.mu.Lock()
		runtime.Monitor.ActiveProxyRequestCount--
		runtime.Monitor.ActiveProxyPathCounts[pathname]--
		if runtime.Monitor.ActiveProxyPathCounts[pathname] <= 0 {
			delete(runtime.Monitor.ActiveProxyPathCounts, pathname)
		}
		runtime.Monitor.mu.Unlock()
	}()

	shouldInspect := matchPath(runtime.Config, pathname)
	requestBody, err := readRequestBody(request, runtime.Config.RequestBodyLimitBytes)
	if err != nil {
		runtime.Monitor.mu.Lock()
		runtime.Monitor.FailedProxyRequestCount++
		runtime.Monitor.mu.Unlock()
		runtime.Logger(fmt.Sprintf("[gateway-reject] request body too large path=%s limit=%d", pathname, runtime.Config.RequestBodyLimitBytes))
		writeGatewayError(writer, err)
		return
	}

	requestJSON := map[string]any(nil)
	if isJSONContentType(request.Header.Get("content-type")) && len(requestBody) > 0 {
		requestJSON = parseJSON(requestBody)
	}
	tracking.RequestJSON = requestJSON
	tracking.RequestKind = detectRequestKind(request.Header, requestJSON)
	tracking.RequestSummary = buildRequestSummary(requestBody, request.Header)
	tracking.RequestReasoningEffort = extractRequestReasoningEffort(requestJSON)
	tracking.RequestModel = strings.TrimSpace(anyToString(requestJSON["model"]))
	tracking.StripEncryptedResponse = true
	if shouldInspect && strings.Contains(pathname, "/responses") {
		tracking.ContinuationBaseJSON = cloneJSONMap(requestJSON)
	}

	requestIsStream := optionalBool(requestJSON["stream"], false)
	upstreamURL, err := buildUpstreamURL(runtime.Config.UpstreamBaseURL, incomingURL)
	if err != nil {
		writeGatewayError(writer, err)
		return
	}

	guardRetryUsed := 0
	for {
		tracking.GuardRetryRemaining = max(0, runtime.Config.GuardRetryAttempts-guardRetryUsed)
		result, handleErr := executeProxyAttempt(runtime, writer, request, upstreamURL, pathname, requestBody, requestJSON, requestIsStream, shouldInspect, tracking)
		if result.ContinuationRecovery && guardRetryUsed < runtime.Config.GuardRetryAttempts {
			guardRetryUsed++
			requestJSON = result.RequestJSON
			requestBody = result.RequestBody
			requestIsStream = true
			continue
		}
		if result.GuardRetry && guardRetryUsed < runtime.Config.GuardRetryAttempts {
			guardRetryUsed++
			continue
		}
		if handleErr != nil {
			writeGatewayError(writer, handleErr)
		}
		return
	}
}

func executeProxyAttempt(runtime *appRuntime, writer http.ResponseWriter, request *http.Request, upstreamURL string, pathname string, requestBody []byte, requestJSON map[string]any, requestIsStream bool, shouldInspect bool, tracking *requestTracking) (handlerResult, error) {
	headers := cloneHeadersForUpstream(request.Header)
	if headers.Get("Authorization") == "" {
		state, _ := readInstallState(runtime.Paths.StatePath)
		codexConfigPath := ""
		stateRoot := runtime.Paths.StateRoot
		if state != nil {
			codexConfigPath = state.CodexConfigPath
			stateRoot = state.StateRoot
		}
		if token, err := resolveBearerToken("", stateRoot, codexConfigPath); err == nil {
			headers.Set("Authorization", token)
		}
	}
	if len(requestBody) > 0 {
		headers.Set("content-length", strconv.Itoa(len(requestBody)))
	}

	ctx, cancel := context.WithTimeout(request.Context(), 5*time.Minute)
	defer cancel()
	upstreamRequest, err := http.NewRequestWithContext(ctx, request.Method, upstreamURL, bytes.NewReader(requestBody))
	if err != nil {
		return handlerResult{}, err
	}
	upstreamRequest.Header = headers
	response, err := http.DefaultClient.Do(upstreamRequest)
	if err != nil {
		runtime.Monitor.mu.Lock()
		runtime.Monitor.FailedProxyRequestCount++
		runtime.Monitor.mu.Unlock()
		runtime.Logger(fmt.Sprintf("[upstream-error] path=%s message=%v", pathname, err))
		return handlerResult{}, &gatewayError{StatusCode: 502, ErrorType: "upstream_error", Code: "upstream_fetch_failed", Message: "upstream fetch failed"}
	}
	defer response.Body.Close()

	if !shouldInspect {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return handlerResult{}, err
		}
		copyHeadersToClient(response.Header, writer)
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(body)
		runtime.Monitor.mu.Lock()
		runtime.Monitor.BypassedProxyRequestCount++
		runtime.Monitor.BypassedProxyPathCounts[pathname]++
		runtime.Monitor.InspectedResponseCount++
		runtime.Monitor.mu.Unlock()
		runtime.appendReasoningSample(reasoningSample{
			ID:               tracking.ID,
			RecordedAt:       time.Now().Format(time.RFC3339),
			Pathname:         pathname,
			Method:           request.Method,
			RequestKind:      tracking.RequestKind,
			RequestModel:     tracking.RequestModel,
			RequestReasoningEffort: tracking.RequestReasoningEffort,
			Streaming:        requestIsStream,
			FinalAction:      "bypassed",
			ClientHTTPStatus: intPointer(response.StatusCode),
			RequestSummary:   tracking.RequestSummary,
		})
		return handlerResult{Handled: true}, nil
	}

	if isSSEContentType(response.Header.Get("content-type")) || (requestIsStream && !isJSONContentType(response.Header.Get("content-type"))) {
		return handleStreaming(runtime, writer, request, response, pathname, tracking)
	}
	return handleNonStreaming(runtime, writer, request, response, pathname, tracking)
}

func handleNonStreaming(runtime *appRuntime, writer http.ResponseWriter, request *http.Request, response *http.Response, pathname string, tracking *requestTracking) (handlerResult, error) {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return handlerResult{}, err
	}
	parsed := map[string]any(nil)
	if isJSONContentType(response.Header.Get("content-type")) {
		parsed = parseJSON(body)
	}
	reasoning := extractReasoningTokens(parsed)
	structure := createStructureAccumulator()
	if parsed != nil {
		applyStructureSignalsFromPayload(parsed, structure, false)
	}
	ruleMatch := buildInterceptRuleMatch(runtime.Config, reasoning, tracking, structure)
	applyMetricsForMatch(runtime, reasoning, ruleMatch, false)

	if isUpstreamCapacityErrorResponse(response, parsed, body) && runtime.Config.RetryUpstreamCapacityErrors && tracking.GuardRetryRemaining > 0 {
		runtime.Logger(fmt.Sprintf("[upstream-capacity] non-stream path=%s status=%d action=internal_retry remaining=%d", pathname, response.StatusCode, tracking.GuardRetryRemaining))
		return handlerResult{GuardRetry: true}, nil
	}

	if ruleMatch.Matched {
		action := fmt.Sprintf("return_status_%d", runtime.Config.NonStreamStatusCode)
		if !runtime.Config.InterceptNonStreaming {
			action = "observe_only"
		} else if tracking.GuardRetryRemaining > 0 {
			action = fmt.Sprintf("internal_retry remaining=%d", tracking.GuardRetryRemaining)
		}
		if runtime.Config.LogMatch {
			runtime.Logger(fmt.Sprintf("[match] non-stream path=%s %s action=%s mode=%s", pathname, ruleMatch.ReasonForLog, action, ruleMatch.Mode))
		}
		if runtime.Config.InterceptNonStreaming {
			if tracking.GuardRetryRemaining > 0 {
				return handlerResult{GuardRetry: true}, nil
			}
			writeBlockedResponse(writer, runtime.Config.NonStreamStatusCode, pathname, reasoning)
			runtime.appendReasoningSample(buildSample(tracking, request, false, reasoning, ruleMatch, "blocked", intPointer(runtime.Config.NonStreamStatusCode), true, structure, nil))
			return handlerResult{Handled: true}, nil
		}
	}

	copyHeadersToClient(response.Header, writer)
	writer.WriteHeader(response.StatusCode)
	if tracking.StripEncryptedResponse {
		body = stripEncryptedContentFromBody(body)
	}
	_, _ = writer.Write(body)
	finalAction := "passed"
	if ruleMatch.Matched && !runtime.Config.InterceptNonStreaming {
		finalAction = "observe_only"
	}
	runtime.appendReasoningSample(buildSample(tracking, request, false, reasoning, ruleMatch, finalAction, intPointer(response.StatusCode), false, structure, nil))
	return handlerResult{Handled: true}, nil
}

func handleStreaming(runtime *appRuntime, writer http.ResponseWriter, request *http.Request, response *http.Response, pathname string, tracking *requestTracking) (handlerResult, error) {
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return handlerResult{}, err
	}
	inspection := inspectSSEBody(runtime.Config, body, tracking)
	applyMetricsForMatch(runtime, inspection.Reasoning, inspection.Matched, true)

	if inspection.Matched.Matched {
		shouldIntercept := runtime.Config.InterceptStreaming
		canContinuationRecover := shouldIntercept &&
			runtime.Config.StreamAction == "continuation_recovery" &&
			tracking.GuardRetryRemaining > 0 &&
			tracking.RequestKind != requestKindContextCompaction &&
			(strings.HasSuffix(pathname, "/responses") || strings.HasSuffix(pathname, "/v1/responses"))
		canGuardRetry := shouldIntercept && !canContinuationRecover && tracking.GuardRetryRemaining > 0
		action := fmt.Sprintf("return_status_%d", runtime.Config.NonStreamStatusCode)
		switch {
		case !shouldIntercept:
			action = "observe_only"
		case canContinuationRecover:
			action = fmt.Sprintf("continuation_recovery remaining=%d", tracking.GuardRetryRemaining)
		case canGuardRetry:
			action = fmt.Sprintf("internal_retry remaining=%d", tracking.GuardRetryRemaining)
		}
		if runtime.Config.LogMatch {
			runtime.Logger(fmt.Sprintf("[match] stream path=%s %s action=%s mode=%s", pathname, inspection.Matched.ReasonForLog, action, inspection.Matched.Mode))
		}
		if shouldIntercept {
			if canContinuationRecover {
				nextJSON, nextBody, buildErr := buildContinuationRecoveryRequestBody(runtime.Config, tracking.ContinuationBaseJSON)
				if buildErr != nil {
					return handlerResult{}, buildErr
				}
				runtime.Monitor.mu.Lock()
				runtime.Monitor.ContinuationRecoveryCount++
				runtime.Monitor.mu.Unlock()
				runtime.appendReasoningSample(buildSample(tracking, request, true, inspection.Reasoning, inspection.Matched, "continuation_recovery", nil, true, inspection.Structure, nil))
				return handlerResult{ContinuationRecovery: true, RequestJSON: nextJSON, RequestBody: nextBody}, nil
			}
			if canGuardRetry {
				runtime.appendReasoningSample(buildSample(tracking, request, true, inspection.Reasoning, inspection.Matched, "internal_retry", nil, true, inspection.Structure, nil))
				return handlerResult{GuardRetry: true}, nil
			}
			if runtime.Config.StreamAction == "disconnect" {
				if hj, ok := writer.(http.Hijacker); ok {
					conn, _, hijackErr := hj.Hijack()
					if hijackErr == nil {
						_ = conn.Close()
						runtime.appendReasoningSample(buildSample(tracking, request, true, inspection.Reasoning, inspection.Matched, "disconnect", nil, true, inspection.Structure, nil))
						return handlerResult{Handled: true}, nil
					}
				}
			}
			writeBlockedResponse(writer, runtime.Config.NonStreamStatusCode, pathname, inspection.Reasoning)
			runtime.appendReasoningSample(buildSample(tracking, request, true, inspection.Reasoning, inspection.Matched, "blocked", intPointer(runtime.Config.NonStreamStatusCode), true, inspection.Structure, nil))
			return handlerResult{Handled: true}, nil
		}
	}

	copyHeadersToClient(response.Header, writer)
	writer.WriteHeader(response.StatusCode)
	if tracking.StripEncryptedResponse {
		body = stripEncryptedContentFromSSEBody(body)
	}
	_, _ = writer.Write(body)
	finalAction := "passed"
	if inspection.Matched.Matched && !runtime.Config.InterceptStreaming {
		finalAction = "observe_only"
	} else if response.StatusCode < 400 {
		runtime.Monitor.mu.Lock()
		runtime.Monitor.ContinuationRecoverySuccessCount++
		runtime.Monitor.mu.Unlock()
	}
	runtime.appendReasoningSample(buildSample(tracking, request, true, inspection.Reasoning, inspection.Matched, finalAction, intPointer(response.StatusCode), false, inspection.Structure, nil))
	return handlerResult{Handled: true}, nil
}

func applyMetricsForMatch(runtime *appRuntime, reasoning *int, ruleMatch interceptRuleMatch, streaming bool) {
	runtime.Monitor.mu.Lock()
	defer runtime.Monitor.mu.Unlock()
	runtime.Monitor.InspectedResponseCount++
	if reasoning != nil {
		runtime.Monitor.ObservedReasoningCounts[strconv.Itoa(*reasoning)]++
	}
	if ruleMatch.Matched {
		runtime.Monitor.MatchedResponseCount++
		if streaming {
			runtime.Monitor.MatchedStreamingCount++
			if runtime.Config.InterceptStreaming {
				runtime.Monitor.BlockedResponseCount++
				runtime.Monitor.BlockedStreamingCount++
			}
		} else {
			runtime.Monitor.MatchedNonStreamingCount++
			if runtime.Config.InterceptNonStreaming {
				runtime.Monitor.BlockedResponseCount++
				runtime.Monitor.BlockedNonStreamingCount++
			}
		}
	}
}

func buildSample(tracking *requestTracking, request *http.Request, streaming bool, reasoning *int, ruleMatch interceptRuleMatch, finalAction string, clientStatus *int, blockedByGateway bool, structure *structureAccumulator, failureSummary map[string]any) reasoningSample {
	structureMap := map[string]any{
		"event_type_counts":         structure.EventTypeCounts,
		"response_item_type_counts": structure.ResponseItemTypeCounts,
		"has_commentary":            structure.HasCommentary,
		"has_final_answer":          structure.HasFinalAnswer,
		"has_tool_call":             structure.HasToolCall,
		"has_output_text":           structure.HasOutputText,
		"has_reasoning_item":        structure.HasReasoningItem,
	}
	return reasoningSample{
		ID:                    tracking.ID,
		RecordedAt:            time.Now().Format(time.RFC3339),
		Pathname:              tracking.Pathname,
		Method:                request.Method,
		RequestKind:           tracking.RequestKind,
		RequestModel:          tracking.RequestModel,
		RequestReasoningEffort: tracking.RequestReasoningEffort,
		Streaming:             streaming,
		ReasoningTokens:       reasoning,
		MatchedCurrentRule:    ruleMatch.Matched,
		FinalAction:           finalAction,
		ClientHTTPStatus:      clientStatus,
		BlockedByGateway:      blockedByGateway,
		InterceptExemptReason: ruleMatch.ExemptReason,
		RequestSummary:        tracking.RequestSummary,
		FailureSummary:        failureSummary,
		Structure:             structureMap,
	}
}

func writeBlockedResponse(writer http.ResponseWriter, statusCode int, pathname string, reasoning *int) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.Header().Set("x-codex-retry-gateway-reason", "reasoning-guard-triggered")
	writer.WriteHeader(statusCode)
	_, _ = writer.Write(buildBlockedBody(pathname, reasoning, statusCode))
}

func copyHeadersToClient(headers http.Header, writer http.ResponseWriter) {
	for key, values := range headers {
		if strings.EqualFold(key, "content-length") || strings.EqualFold(key, "connection") || strings.EqualFold(key, "transfer-encoding") {
			continue
		}
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
}

func writeGatewayError(writer http.ResponseWriter, err error) {
	statusCode := 502
	errorType := "codex_retry_gateway_error"
	code := "gateway_error"
	message := err.Error()
	var gatewayErr *gatewayError
	if errors.As(err, &gatewayErr) {
		if gatewayErr.StatusCode != 0 {
			statusCode = gatewayErr.StatusCode
		}
		if gatewayErr.ErrorType != "" {
			errorType = gatewayErr.ErrorType
		}
		if gatewayErr.Code != "" {
			code = gatewayErr.Code
		}
		if gatewayErr.Message != "" {
			message = gatewayErr.Message
		}
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"code":    code,
		},
	})
	writeJSONResponse(writer, statusCode, body)
}

func intPointer(value int) *int {
	return &value
}

func max(left int, right int) int {
	if left > right {
		return left
	}
	return right
}
