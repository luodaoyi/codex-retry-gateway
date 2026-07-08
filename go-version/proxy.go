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
	"regexp"
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
	ID                                  string
	Pathname                            string
	Method                              string
	RequestKind                         string
	RequestJSON                         map[string]any
	RequestSummary                      map[string]any
	RequestPayloadExcerpt               string
	RequestStartedAt                    time.Time
	GuardRetryRemaining                 int
	AttemptIndex                        int
	ContinuationBaseJSON                map[string]any
	StripEncryptedResponse              bool
	RequestReasoningEffort              string
	RequestModel                        string
	ModelContext                        *requestModelContext
	LastPayload                         map[string]any
	UpstreamHTTPStatus                  *int
	UpstreamStreamTerminated            bool
	Outcome                             string
	ContinuationRecoveryAttempted       bool
	ContinuationRecoverySuccessRecorded bool
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

var (
	sseBlockSplitPattern        = regexp.MustCompile(`\r?\n\r?\n`)
	sseLineSplitPattern         = regexp.MustCompile(`\r?\n`)
	encryptedContentTextPattern = regexp.MustCompile(`(?i)encrypted_content`)
)

type sseChunkState struct {
	buffer []byte
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

func extractInputTokens(payload map[string]any) *int {
	pointers := [][]string{
		{"usage", "input_tokens"},
		{"input_tokens"},
	}
	return extractFirstNestedInt(payload, pointers)
}

func extractOutputTokens(payload map[string]any) *int {
	pointers := [][]string{
		{"usage", "output_tokens"},
		{"output_tokens"},
	}
	return extractFirstNestedInt(payload, pointers)
}

func extractTotalTokens(payload map[string]any) *int {
	pointers := [][]string{
		{"usage", "total_tokens"},
		{"total_tokens"},
	}
	return extractFirstNestedInt(payload, pointers)
}

func extractFirstNestedInt(payload map[string]any, pointers [][]string) *int {
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
		stringifyRequestKindSignal(requestJSON["metadata"]),
		stringifyRequestKindSignal(requestJSON["codex_request_kind"]),
		stringifyRequestKindSignal(requestJSON["request_kind"]),
		stringifyRequestKindSignal(requestJSON["purpose"]),
	}
	if includesAnyContextCompactionMarker(strings.ToLower(strings.Join(metadataParts, " "))) {
		return requestKindContextCompaction
	}
	return requestKindNormal
}

func stringifyRequestKindSignal(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string, fmt.Stringer, json.Number, bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return anyToString(typed)
	default:
		body, err := json.Marshal(typed)
		if err == nil {
			return string(body)
		}
		return anyToString(typed)
	}
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

func buildGatewayErrorBody(message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "codex_retry_gateway_error",
			"code":    "gateway_error",
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

func isResponsesPath(pathname string) bool {
	return pathname == "/responses" || pathname == "/v1/responses"
}

func textMayContainEncryptedContent(text string) bool {
	return encryptedContentTextPattern.MatchString(text)
}

func redactEncryptedContentText(text string) string {
	return encryptedContentTextPattern.ReplaceAllString(text, "redacted_sensitive_content")
}

func isExpectedStreamTerminationError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	message := strings.TrimSpace(strings.ToLower(err.Error()))
	return message == "terminated" || strings.HasSuffix(message, ": terminated")
}

func closeClientConnection(writer http.ResponseWriter) bool {
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		return false
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func shouldStripEncryptedContentFromContinuationResponse(config gatewayConfig, pathname string, shouldInspect bool, requestJSON map[string]any) bool {
	return shouldInspect &&
		isResponsesPath(pathname) &&
		optionalBool(requestJSON["stream"], false) &&
		normalizeStreamAction(config.StreamAction, defaultStreamAction) == "continuation_recovery"
}

func stripEncryptedContentFromSSEBody(body []byte) []byte {
	blocks := sseBlockSplitPattern.Split(string(body), -1)
	result := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block == "" {
			result = append(result, block)
			continue
		}
		lines := sseLineSplitPattern.Split(block, -1)
		sanitizedNonDataLines := make([]string, 0, len(lines))
		originalDataLines := make([]string, 0, len(lines))
		nonDataWasRedacted := false
		for _, line := range lines {
			if strings.HasPrefix(line, "data:") {
				originalDataLines = append(originalDataLines, line)
				continue
			}
			sanitized := line
			if textMayContainEncryptedContent(line) {
				sanitized = redactEncryptedContentText(line)
			}
			if sanitized != line {
				nonDataWasRedacted = true
			}
			sanitizedNonDataLines = append(sanitizedNonDataLines, sanitized)
		}
		rebuildWithOriginalData := func() string {
			outputLines := make([]string, 0, len(sanitizedNonDataLines)+len(originalDataLines))
			outputLines = append(outputLines, sanitizedNonDataLines...)
			outputLines = append(outputLines, originalDataLines...)
			return strings.Join(outputLines, "\n")
		}
		if len(originalDataLines) == 0 {
			if textMayContainEncryptedContent(block) {
				result = append(result, redactEncryptedContentText(block))
			} else {
				result = append(result, block)
			}
			continue
		}
		dataLines := make([]string, 0, len(originalDataLines))
		for _, line := range originalDataLines {
			payloadText := strings.TrimPrefix(line, "data:")
			payloadText = strings.TrimPrefix(payloadText, " ")
			dataLines = append(dataLines, payloadText)
		}
		payloadText := strings.Join(dataLines, "\n")
		if payloadText == "[DONE]" {
			if nonDataWasRedacted {
				result = append(result, rebuildWithOriginalData())
			} else {
				result = append(result, block)
			}
			continue
		}
		var parsed any
		if err := json.Unmarshal([]byte(payloadText), &parsed); err == nil {
			redacted, _ := json.Marshal(stripEncryptedContent(parsed))
			outputLines := make([]string, 0, len(sanitizedNonDataLines)+1)
			outputLines = append(outputLines, sanitizedNonDataLines...)
			outputLines = append(outputLines, "data: "+string(redacted))
			result = append(result, strings.Join(outputLines, "\n"))
			continue
		}
		if textMayContainEncryptedContent(payloadText) {
			redacted, _ := json.Marshal(map[string]any{"type": "gateway.redacted", "redacted": true})
			outputLines := make([]string, 0, len(sanitizedNonDataLines)+1)
			outputLines = append(outputLines, sanitizedNonDataLines...)
			outputLines = append(outputLines, "data: "+string(redacted))
			result = append(result, strings.Join(outputLines, "\n"))
			continue
		}
		if nonDataWasRedacted {
			result = append(result, rebuildWithOriginalData())
		} else {
			result = append(result, block)
		}
	}
	return []byte(strings.Join(result, "\n\n"))
}

type streamInspection struct {
	Reasoning *int
	Structure *structureAccumulator
	Payloads  []map[string]any
	Matched   interceptRuleMatch
}

func inspectSSEBody(config gatewayConfig, body []byte, tracking *requestTracking) streamInspection {
	structure := createStructureAccumulator()
	var observedReasoning *int
	payloads := []map[string]any{}
	blocks := sseBlockSplitPattern.Split(string(body), -1)
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		lines := sseLineSplitPattern.Split(block, -1)
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

func extractSSEBlocks(buffer []byte) ([][]byte, []byte) {
	blocks := make([][]byte, 0)
	start := 0
	for start < len(buffer) {
		remaining := buffer[start:]
		crlfIndex := bytes.Index(remaining, []byte("\r\n\r\n"))
		lfIndex := bytes.Index(remaining, []byte("\n\n"))
		separatorIndex := -1
		separatorLength := 0
		switch {
		case crlfIndex >= 0 && (lfIndex < 0 || crlfIndex <= lfIndex):
			separatorIndex = start + crlfIndex
			separatorLength = 4
		case lfIndex >= 0:
			separatorIndex = start + lfIndex
			separatorLength = 2
		default:
			return blocks, buffer[start:]
		}
		block := append([]byte(nil), buffer[start:separatorIndex]...)
		blocks = append(blocks, block)
		start = separatorIndex + separatorLength
	}
	return blocks, nil
}

func splitSSELines(block []byte) [][]byte {
	rawLines := bytes.Split(block, []byte{'\n'})
	lines := make([][]byte, 0, len(rawLines))
	for _, line := range rawLines {
		lines = append(lines, bytes.TrimSuffix(line, []byte{'\r'}))
	}
	return lines
}

func parseSSEPayloads(state *sseChunkState, chunk []byte) []map[string]any {
	state.buffer = append(state.buffer, chunk...)
	blocks, remainder := extractSSEBlocks(state.buffer)
	state.buffer = append(state.buffer[:0], remainder...)
	payloads := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		lines := splitSSELines(block)
		dataLines := make([]string, 0, len(lines))
		for _, line := range lines {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			dataLine := bytes.TrimPrefix(line, []byte("data:"))
			dataLine = bytes.TrimPrefix(dataLine, []byte(" "))
			dataLines = append(dataLines, string(dataLine))
		}
		if len(dataLines) == 0 {
			continue
		}
		payloadText := strings.Join(dataLines, "\n")
		if payloadText == "[DONE]" {
			continue
		}
		if payload := parseJSON([]byte(payloadText)); payload != nil {
			payloads = append(payloads, payload)
		}
	}
	return payloads
}

func inspectSSEChunk(state *sseChunkState, chunk []byte) (*int, []map[string]any) {
	payloads := parseSSEPayloads(state, chunk)
	var reasoning *int
	for _, payload := range payloads {
		if extracted := extractReasoningTokens(payload); extracted != nil {
			reasoning = extracted
		}
	}
	return reasoning, payloads
}

func buildContinuationRecoveryFoldedBody(_ *requestTracking, finalBody []byte) []byte {
	return finalBody
}

func setRequestTrackingOutcome(tracking *requestTracking, outcome string) {
	if tracking == nil {
		return
	}
	tracking.Outcome = outcome
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
	tracking.RequestPayloadExcerpt = buildRequestPayloadExcerpt(requestBody)
	tracking.RequestReasoningEffort = extractRequestReasoningEffort(requestJSON)
	tracking.RequestModel = strings.TrimSpace(anyToString(requestJSON["model"]))
	requestIsStream := optionalBool(requestJSON["stream"], false)
	if shouldInspect && isResponsesPath(pathname) && requestIsStream && requestJSON != nil {
		tracking.ContinuationBaseJSON = cloneJSONMap(requestJSON)
	}
	tracking.StripEncryptedResponse = shouldStripEncryptedContentFromContinuationResponse(runtime.Config, pathname, shouldInspect, requestJSON)
	upstreamURL, err := buildUpstreamURL(runtime.Config.UpstreamBaseURL, incomingURL)
	if err != nil {
		writeGatewayError(writer, err)
		return
	}

	guardRetryUsed := 0
	for {
		tracking.AttemptIndex = guardRetryUsed
		tracking.GuardRetryRemaining = max(0, runtime.Config.GuardRetryAttempts-guardRetryUsed)
		result, handleErr := executeProxyAttempt(runtime, writer, request, upstreamURL, pathname, requestBody, requestJSON, requestIsStream, shouldInspect, tracking)
		if result.ContinuationRecovery && guardRetryUsed < runtime.Config.GuardRetryAttempts {
			guardRetryUsed++
			requestJSON = result.RequestJSON
			requestBody = result.RequestBody
			requestIsStream = true
			tracking.RequestJSON = requestJSON
			tracking.StripEncryptedResponse = true
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
	if len(requestBody) > 0 {
		headers.Set("content-length", strconv.Itoa(len(requestBody)))
	}
	tracking.ModelContext = createRequestModelContext("", tracking.RequestModel)
	tracking.RequestStartedAt = time.Now()
	tracking.LastPayload = nil
	tracking.UpstreamHTTPStatus = nil
	tracking.UpstreamStreamTerminated = false

	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()

	var response *http.Response
	var err error
	for attempt := 1; attempt <= 2; attempt++ {
		var upstreamRequest *http.Request
		upstreamRequest, err = http.NewRequestWithContext(ctx, request.Method, upstreamURL, bytes.NewReader(requestBody))
		if err != nil {
			return handlerResult{}, err
		}
		upstreamRequest.Header = headers.Clone()
		response, err = http.DefaultClient.Do(upstreamRequest)
		if err == nil {
			break
		}
		if request.Context().Err() != nil || errors.Is(err, context.Canceled) || attempt == 2 {
			runtime.Monitor.mu.Lock()
			runtime.Monitor.FailedProxyRequestCount++
			runtime.Monitor.mu.Unlock()
			runtime.Logger(fmt.Sprintf("[upstream-error] path=%s message=%v", pathname, err))
			return handlerResult{}, &gatewayError{StatusCode: 502, ErrorType: "upstream_error", Code: "upstream_fetch_failed", Message: "upstream fetch failed"}
		}
		runtime.Logger(fmt.Sprintf("[retry] upstream fetch failed attempt=%d url=%s", attempt, upstreamURL))
	}
	defer response.Body.Close()
	tracking.UpstreamHTTPStatus = intPointer(response.StatusCode)

	if !shouldInspect {
		body, err := io.ReadAll(response.Body)
		if err != nil {
			return handlerResult{}, err
		}
		var parsed map[string]any
		if isJSONContentType(response.Header.Get("content-type")) {
			parsed = parseJSON(body)
			tracking.LastPayload = parsed
			applyPayloadModelSignals(tracking.ModelContext, parsed, false, true)
		}
		finalizeModelInsights(runtime.Monitor, pathname, tracking.ModelContext, parsed)
		copyHeadersToClient(response.Header, writer)
		writer.WriteHeader(response.StatusCode)
		_, _ = writer.Write(body)
		runtime.Monitor.mu.Lock()
		runtime.Monitor.BypassedProxyRequestCount++
		runtime.Monitor.BypassedProxyPathCounts[pathname]++
		runtime.Monitor.InspectedResponseCount++
		runtime.Monitor.mu.Unlock()
		setRequestTrackingOutcome(tracking, "bypassed")
		sample := reasoningSample{
			ID:                     tracking.ID,
			RecordedAt:             time.Now().Format(time.RFC3339),
			Pathname:               pathname,
			Method:                 request.Method,
			RequestKind:            tracking.RequestKind,
			RequestModel:           tracking.RequestModel,
			RequestReasoningEffort: tracking.RequestReasoningEffort,
			Streaming:              requestIsStream,
			FinalAction:            "bypassed",
			ClientHTTPStatus:       intPointer(response.StatusCode),
			RequestSummary:         tracking.RequestSummary,
		}
		applyModelContextToReasoningSample(&sample, tracking.ModelContext)
		runtime.appendReasoningSample(sample)
		return handlerResult{Handled: true}, nil
	}

	if isSSEContentType(response.Header.Get("content-type")) || (requestIsStream && !isJSONContentType(response.Header.Get("content-type"))) {
		return handleStreaming(runtime, writer, request, response, pathname, tracking, cancel)
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
	tracking.LastPayload = parsed
	applyPayloadModelSignals(tracking.ModelContext, parsed, false, true)
	defer finalizeModelInsights(runtime.Monitor, pathname, tracking.ModelContext, parsed)
	reasoning := extractReasoningTokens(parsed)
	structure := createStructureAccumulator()
	if parsed != nil {
		applyStructureSignalsFromPayload(parsed, structure, false)
	}
	ruleMatch := buildInterceptRuleMatch(runtime.Config, reasoning, tracking, structure)
	recordInspectedResponse(runtime.Monitor, reasoning, ruleMatch.Matched, "non-stream")
	setRequestTrackingOutcome(tracking, "inspected")

	if isUpstreamCapacityErrorResponse(response, parsed, body) && runtime.Config.RetryUpstreamCapacityErrors {
		canGuardRetry := tracking.GuardRetryRemaining > 0
		if runtime.Config.LogMatch {
			action := "pass_through"
			if canGuardRetry {
				action = fmt.Sprintf("internal_retry remaining=%d", tracking.GuardRetryRemaining)
			}
			runtime.Logger(fmt.Sprintf("[upstream-capacity] non-stream path=%s status=%d action=%s", pathname, response.StatusCode, action))
		}
		if canGuardRetry {
			recordBlockedResponse(runtime.Monitor, "non-stream")
			runtime.appendReasoningSample(buildSample(tracking, request, false, reasoning, ruleMatch, "upstream_capacity_internal_retry", nil, true, structure, nil))
			return handlerResult{GuardRetry: true}, nil
		}
	}

	if ruleMatch.Matched {
		shouldIntercept := runtime.Config.InterceptNonStreaming
		canGuardRetry := shouldIntercept && tracking.GuardRetryRemaining > 0
		action := fmt.Sprintf("return_status_%d", runtime.Config.NonStreamStatusCode)
		if !shouldIntercept {
			action = "observe_only"
		} else if canGuardRetry {
			action = fmt.Sprintf("internal_retry remaining=%d", tracking.GuardRetryRemaining)
		}
		if runtime.Config.LogMatch {
			runtime.Logger(fmt.Sprintf("[match] non-stream path=%s %s action=%s mode=%s", pathname, ruleMatch.ReasonForLog, action, ruleMatch.Mode))
		}
		if shouldIntercept {
			recordBlockedResponse(runtime.Monitor, "non-stream")
			if canGuardRetry {
				runtime.appendReasoningSample(buildSample(tracking, request, false, reasoning, ruleMatch, "internal_retry", nil, true, structure, nil))
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
	} else if !ruleMatch.Matched && response.StatusCode < 400 {
		recordContinuationRecoverySuccess(runtime.Monitor, tracking)
	}
	runtime.appendReasoningSample(buildSample(tracking, request, false, reasoning, ruleMatch, finalAction, intPointer(response.StatusCode), false, structure, nil))
	return handlerResult{Handled: true}, nil
}

func handleStreaming(runtime *appRuntime, writer http.ResponseWriter, request *http.Request, response *http.Response, pathname string, tracking *requestTracking, cancel context.CancelFunc) (handlerResult, error) {
	defer finalizeModelInsights(runtime.Monitor, pathname, tracking.ModelContext, nil)
	streamAction := normalizeStreamAction(runtime.Config.StreamAction, defaultStreamAction)
	strict502Mode := streamAction != "disconnect"
	state := &sseChunkState{}
	structure := createStructureAccumulator()
	var observedReasoning *int
	inspectedRecorded := false
	observedMatchedRule := false
	observedOnlyMatchedRule := false
	wroteAnyChunk := false
	upstreamHeaderWritten := false
	bufferedChunks := make([][]byte, 0)

	if !strict502Mode {
		copyHeadersToClient(response.Header, writer)
	}

	writeUpstreamHeadersIfNeeded := func() {
		if !upstreamHeaderWritten {
			writer.WriteHeader(response.StatusCode)
			upstreamHeaderWritten = true
		}
	}

	chunkBuffer := make([]byte, 32*1024)
	for {
		n, readErr := response.Body.Read(chunkBuffer)
		if n > 0 {
			chunk := append([]byte(nil), chunkBuffer[:n]...)
			reasoning, payloads := inspectSSEChunk(state, chunk)
			for _, payload := range payloads {
				tracking.LastPayload = payload
				applyStructureSignalsFromPayload(payload, structure, true)
				applyPayloadModelSignals(tracking.ModelContext, payload, true, false)
			}
			if reasoning != nil {
				observedReasoning = reasoning
			}
			ruleMatch := buildInterceptRuleMatch(runtime.Config, reasoning, tracking, structure)
			if ruleMatch.Mode == defaultInterceptRuleMode && ruleMatch.Matched {
				if !inspectedRecorded {
					recordInspectedResponse(runtime.Monitor, reasoning, true, "stream")
					inspectedRecorded = true
				}
				setRequestTrackingOutcome(tracking, "inspected")
				shouldIntercept := runtime.Config.InterceptStreaming
				canReturnBlockedStatus := strict502Mode || !wroteAnyChunk
				canContinuationRecover := shouldIntercept &&
					canReturnBlockedStatus &&
					streamAction == "continuation_recovery" &&
					tracking.GuardRetryRemaining > 0 &&
					tracking.RequestKind != requestKindContextCompaction &&
					isResponsesPath(pathname)
				canGuardRetry := shouldIntercept && canReturnBlockedStatus && !canContinuationRecover && tracking.GuardRetryRemaining > 0
				if runtime.Config.LogMatch {
					action := "disconnect"
					switch {
					case !shouldIntercept:
						action = "observe_only"
					case canContinuationRecover:
						action = fmt.Sprintf("continuation_recovery remaining=%d", tracking.GuardRetryRemaining)
					case canGuardRetry:
						action = fmt.Sprintf("internal_retry remaining=%d", tracking.GuardRetryRemaining)
					case canReturnBlockedStatus:
						action = fmt.Sprintf("return_status_%d", runtime.Config.NonStreamStatusCode)
					}
					runtime.Logger(fmt.Sprintf("[match] stream path=%s %s action=%s mode=%s", pathname, ruleMatch.ReasonForLog, action, ruleMatch.Mode))
				}
				if !shouldIntercept {
					observedMatchedRule = true
					observedOnlyMatchedRule = true
					if strict502Mode {
						bufferedChunks = append(bufferedChunks, chunk)
					} else {
						writeUpstreamHeadersIfNeeded()
						wroteAnyChunk = true
						_, _ = writer.Write(chunk)
					}
				} else {
					recordBlockedResponse(runtime.Monitor, "stream")
					cancel()
					_ = response.Body.Close()
					if canReturnBlockedStatus {
						if canContinuationRecover {
							nextJSON, nextBody, buildErr := buildContinuationRecoveryRequestBody(runtime.Config, tracking.ContinuationBaseJSON)
							if buildErr != nil {
								return handlerResult{}, buildErr
							}
							recordContinuationRecoveryAttempt(runtime.Monitor, tracking)
							runtime.appendReasoningSample(buildSample(tracking, request, true, reasoning, ruleMatch, "continuation_recovery", nil, true, structure, nil))
							return handlerResult{ContinuationRecovery: true, RequestJSON: nextJSON, RequestBody: nextBody}, nil
						}
						if canGuardRetry {
							runtime.appendReasoningSample(buildSample(tracking, request, true, reasoning, ruleMatch, "internal_retry", nil, true, structure, nil))
							return handlerResult{GuardRetry: true}, nil
						}
						writeBlockedResponse(writer, runtime.Config.NonStreamStatusCode, pathname, reasoning)
						runtime.appendReasoningSample(buildSample(tracking, request, true, reasoning, ruleMatch, "blocked", intPointer(runtime.Config.NonStreamStatusCode), true, structure, nil))
						return handlerResult{Handled: true}, nil
					}
					closeClientConnection(writer)
					runtime.appendReasoningSample(buildSample(tracking, request, true, reasoning, ruleMatch, "disconnect", nil, true, structure, nil))
					return handlerResult{Handled: true}, nil
				}
			} else {
				if strict502Mode {
					bufferedChunks = append(bufferedChunks, chunk)
				} else {
					writeUpstreamHeadersIfNeeded()
					wroteAnyChunk = true
					_, _ = writer.Write(chunk)
				}
			}
		}

		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			if !isExpectedStreamTerminationError(readErr) {
				if upstreamHeaderWritten || wroteAnyChunk {
					runtime.Logger(fmt.Sprintf("[error] stream read failed after response started path=%s message=%v", pathname, readErr))
					_ = response.Body.Close()
					closeClientConnection(writer)
					return handlerResult{Handled: true}, nil
				}
				return handlerResult{}, readErr
			}
			if !inspectedRecorded {
				recordInspectedResponse(runtime.Monitor, observedReasoning, false, "stream")
				inspectedRecorded = true
			}
			tracking.UpstreamStreamTerminated = true
			setRequestTrackingOutcome(tracking, "inspected")
			if strict502Mode {
				runtime.Logger(fmt.Sprintf("[stream] upstream terminated before completion path=%s action=status_502", pathname))
				writer.Header().Set("content-type", "application/json; charset=utf-8")
				writer.WriteHeader(http.StatusBadGateway)
				_, _ = writer.Write(buildGatewayErrorBody("upstream stream terminated before completion"))
				runtime.appendReasoningSample(buildSample(tracking, request, true, observedReasoning, interceptRuleMatch{}, "upstream_stream_terminated", intPointer(http.StatusBadGateway), false, structure, nil))
			} else {
				writeUpstreamHeadersIfNeeded()
				runtime.appendReasoningSample(buildSample(tracking, request, true, observedReasoning, interceptRuleMatch{}, "upstream_stream_terminated", intPointer(response.StatusCode), false, structure, nil))
			}
			return handlerResult{Handled: true}, nil
		}

		finalRuleMatch := buildInterceptRuleMatch(runtime.Config, observedReasoning, tracking, structure)
		if !inspectedRecorded && finalRuleMatch.Matched {
			recordInspectedResponse(runtime.Monitor, observedReasoning, true, "stream")
			inspectedRecorded = true
			setRequestTrackingOutcome(tracking, "inspected")
			shouldIntercept := runtime.Config.InterceptStreaming
			canReturnBlockedStatus := strict502Mode
			canContinuationRecover := shouldIntercept &&
				canReturnBlockedStatus &&
				finalRuleMatch.Mode == defaultInterceptRuleMode &&
				streamAction == "continuation_recovery" &&
				tracking.GuardRetryRemaining > 0 &&
				tracking.RequestKind != requestKindContextCompaction &&
				isResponsesPath(pathname)
			canGuardRetry := shouldIntercept && canReturnBlockedStatus && !canContinuationRecover && tracking.GuardRetryRemaining > 0
			if runtime.Config.LogMatch {
				action := "observe_only"
				switch {
				case shouldIntercept && canContinuationRecover:
					action = fmt.Sprintf("continuation_recovery remaining=%d", tracking.GuardRetryRemaining)
				case shouldIntercept && canGuardRetry:
					action = fmt.Sprintf("internal_retry remaining=%d", tracking.GuardRetryRemaining)
				case shouldIntercept && canReturnBlockedStatus:
					action = fmt.Sprintf("return_status_%d", runtime.Config.NonStreamStatusCode)
				}
				runtime.Logger(fmt.Sprintf("[match] stream path=%s %s action=%s mode=%s", pathname, finalRuleMatch.ReasonForLog, action, finalRuleMatch.Mode))
			}
			observedMatchedRule = true
			if shouldIntercept && canReturnBlockedStatus {
				recordBlockedResponse(runtime.Monitor, "stream")
				if canContinuationRecover {
					nextJSON, nextBody, buildErr := buildContinuationRecoveryRequestBody(runtime.Config, tracking.ContinuationBaseJSON)
					if buildErr != nil {
						return handlerResult{}, buildErr
					}
					recordContinuationRecoveryAttempt(runtime.Monitor, tracking)
					runtime.appendReasoningSample(buildSample(tracking, request, true, observedReasoning, finalRuleMatch, "continuation_recovery", nil, true, structure, nil))
					return handlerResult{ContinuationRecovery: true, RequestJSON: nextJSON, RequestBody: nextBody}, nil
				}
				if canGuardRetry {
					runtime.appendReasoningSample(buildSample(tracking, request, true, observedReasoning, finalRuleMatch, "internal_retry", nil, true, structure, nil))
					return handlerResult{GuardRetry: true}, nil
				}
				writeBlockedResponse(writer, runtime.Config.NonStreamStatusCode, pathname, observedReasoning)
				runtime.appendReasoningSample(buildSample(tracking, request, true, observedReasoning, finalRuleMatch, "blocked", intPointer(runtime.Config.NonStreamStatusCode), true, structure, nil))
				return handlerResult{Handled: true}, nil
			}
			observedOnlyMatchedRule = true
		}
		if !inspectedRecorded {
			recordInspectedResponse(runtime.Monitor, observedReasoning, false, "stream")
			inspectedRecorded = true
		}
		setRequestTrackingOutcome(tracking, "inspected")
		if strict502Mode {
			copyHeadersToClient(response.Header, writer)
			writer.WriteHeader(response.StatusCode)
			finalBody := buildContinuationRecoveryFoldedBody(tracking, bytes.Join(bufferedChunks, nil))
			if tracking.StripEncryptedResponse {
				finalBody = stripEncryptedContentFromSSEBody(finalBody)
			}
			_, _ = writer.Write(finalBody)
		} else if !upstreamHeaderWritten {
			writeUpstreamHeadersIfNeeded()
		}
		if !observedOnlyMatchedRule && response.StatusCode < 400 {
			recordContinuationRecoverySuccess(runtime.Monitor, tracking)
		}
		sampleRuleMatch := finalRuleMatch
		sampleRuleMatch.Matched = observedMatchedRule
		finalAction := "passed"
		if observedOnlyMatchedRule {
			finalAction = "observe_only"
		}
		runtime.appendReasoningSample(buildSample(tracking, request, true, observedReasoning, sampleRuleMatch, finalAction, intPointer(response.StatusCode), false, structure, nil))
		return handlerResult{Handled: true}, nil
	}
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
	finalAnswerOnly := structure.HasFinalAnswer && !structure.HasCommentary && !structure.HasToolCall && !structure.HasReasoningItem
	finishedAt := time.Now()
	requestStartedAt := tracking.RequestStartedAt
	if requestStartedAt.IsZero() {
		requestStartedAt = finishedAt
	}
	durationMS := int(finishedAt.Sub(requestStartedAt).Milliseconds())
	if durationMS < 0 {
		durationMS = 0
	}
	inputTokens := extractInputTokens(tracking.LastPayload)
	outputTokens := extractOutputTokens(tracking.LastPayload)
	totalTokens := extractTotalTokens(tracking.LastPayload)
	if totalTokens == nil {
		computed := 0
		if inputTokens != nil {
			computed += *inputTokens
		}
		if reasoning != nil {
			computed += *reasoning
		}
		if outputTokens != nil {
			computed += *outputTokens
		}
		if computed > 0 {
			totalTokens = &computed
		}
	}
	var outputTPS any
	var reasoningAdjustedTPS any
	var timeNormalizationDeviation any
	if durationMS > 0 {
		if outputTokens != nil {
			outputTPS = roundMetric((float64(*outputTokens)*1000)/float64(durationMS), 4)
		}
		observedTokens := 0
		if totalTokens != nil {
			observedTokens = *totalTokens
		} else {
			if reasoning != nil {
				observedTokens += *reasoning
			}
			if outputTokens != nil {
				observedTokens += *outputTokens
			}
		}
		if observedTokens > 0 {
			reasoningAdjustedTPS = roundMetric((float64(observedTokens)*1000)/float64(durationMS), 4)
			msPerToken := float64(durationMS) / float64(observedTokens)
			timeNormalizationDeviation = roundMetric(maxFloat(0, (35-msPerToken)/35), 4)
		}
	}
	internalRetryRemaining := tracking.GuardRetryRemaining
	structureMap := map[string]any{
		"event_type_counts":         structure.EventTypeCounts,
		"response_item_type_counts": structure.ResponseItemTypeCounts,
		"has_commentary":            structure.HasCommentary,
		"has_final_answer":          structure.HasFinalAnswer,
		"has_tool_call":             structure.HasToolCall,
		"has_output_text":           structure.HasOutputText,
		"has_reasoning_item":        structure.HasReasoningItem,
	}
	sample := reasoningSample{
		ID:                         tracking.ID,
		SampleID:                   fmt.Sprintf("%s:attempt:%d", tracking.ID, tracking.AttemptIndex+1),
		GatewayRequestID:           tracking.ID,
		AttemptID:                  fmt.Sprintf("%s:attempt:%d", tracking.ID, tracking.AttemptIndex+1),
		RecordedAt:                 time.Now().Format(time.RFC3339),
		TS:                         requestStartedAt.Format(time.RFC3339),
		Pathname:                   tracking.Pathname,
		Path:                       tracking.Pathname,
		Method:                     request.Method,
		RequestKind:                tracking.RequestKind,
		RequestModel:               tracking.RequestModel,
		RequestModelFamily:         normalizeModelFamily(tracking.RequestModel),
		EffectiveLocalModel:        tracking.RequestModel,
		EffectiveLocalModelFamily:  normalizeModelFamily(tracking.RequestModel),
		RequestReasoningEffort:     tracking.RequestReasoningEffort,
		RequestPayloadExcerpt:      tracking.RequestPayloadExcerpt,
		RequestStartedAt:           requestStartedAt.Format(time.RFC3339),
		RequestFinishedAt:          finishedAt.Format(time.RFC3339),
		DurationTotalMS:            &durationMS,
		InputTokens:                inputTokens,
		Streaming:                  streaming,
		ReasoningTokens:            reasoning,
		OutputTokens:               outputTokens,
		TotalTokens:                totalTokens,
		OutputTPS:                  outputTPS,
		ReasoningAdjustedTPS:       reasoningAdjustedTPS,
		TimeNormalizationDeviation: timeNormalizationDeviation,
		MatchedCurrentRule:         ruleMatch.Matched,
		FinalAction:                finalAction,
		UpstreamHTTPStatus:         tracking.UpstreamHTTPStatus,
		ClientHTTPStatus:           clientStatus,
		BlockedByGateway:           blockedByGateway,
		UpstreamStreamTerminated:   tracking.UpstreamStreamTerminated,
		InternalRetryAttemptIndex:  tracking.AttemptIndex,
		InternalRetryRemaining:     &internalRetryRemaining,
		InterceptExemptReason:      ruleMatch.ExemptReason,
		FinalAnswerOnly:            finalAnswerOnly,
		CommentaryObserved:         structure.HasCommentary,
		CommentaryNotObserved:      !structure.HasCommentary,
		HasCommentary:              structure.HasCommentary,
		HasFinalAnswer:             structure.HasFinalAnswer,
		HasToolCall:                structure.HasToolCall,
		HasReasoningItem:           structure.HasReasoningItem,
		RequestSummary:             tracking.RequestSummary,
		FailureSummary:             failureSummary,
		Structure:                  structureMap,
		Extra: map[string]any{
			"duration_total_ms":            durationMS,
			"output_tps":                   outputTPS,
			"reasoning_adjusted_tps":       reasoningAdjustedTPS,
			"time_normalization_deviation": timeNormalizationDeviation,
			"output_tokens":                reasoningValue(outputTokens),
			"total_tokens":                 reasoningValue(totalTokens),
			"internal_retry_attempt_index": tracking.AttemptIndex,
			"internal_retry_remaining":     internalRetryRemaining,
			"upstream_http_status":         reasoningValue(tracking.UpstreamHTTPStatus),
			"upstream_stream_terminated":   tracking.UpstreamStreamTerminated,
		},
	}
	applyModelContextToReasoningSample(&sample, tracking.ModelContext)
	return sample
}

func writeBlockedResponse(writer http.ResponseWriter, statusCode int, pathname string, reasoning *int) {
	writer.Header().Set("content-type", "application/json; charset=utf-8")
	writer.Header().Set("x-codex-retry-gateway-reason", "reasoning-guard-triggered")
	writer.WriteHeader(statusCode)
	_, _ = writer.Write(buildBlockedBody(pathname, reasoning, statusCode))
}

func copyHeadersToClient(headers http.Header, writer http.ResponseWriter) {
	for key, values := range headers {
		if strings.EqualFold(key, "content-length") || strings.EqualFold(key, "connection") || strings.EqualFold(key, "transfer-encoding") || strings.EqualFold(key, "content-encoding") {
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

func maxFloat(left float64, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
