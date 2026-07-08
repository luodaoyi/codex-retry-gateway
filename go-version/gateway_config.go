package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultContinuationMarkerText = "Continue thinking..."
	defaultRequestBodyLimitBytes  = 100 * 1024 * 1024
	legacyRequestBodyLimitBytes   = 10 * 1024 * 1024
	defaultStreamAction           = "continuation_recovery"
	defaultInterceptRuleMode      = "reasoning_tokens"
	defaultReasoningMatchMode     = "formula_518n_minus_2"
	finalOnlyRuleMode             = "final_answer_only_high_xhigh"
	requestKindNormal             = "normal"
	requestKindContextCompaction  = "context_compaction"
)

var defaultEndpoints = []string{
	"/responses",
	"/chat/completions",
	"/v1/responses",
	"/v1/chat/completions",
}

var defaultReasoningEquals = []int{516, 1034, 1552}
var finalOnlyInterceptEfforts = map[string]bool{"high": true, "xhigh": true}
var contextCompactionMarkers = []string{"remote_compaction", "context_compaction"}

type activeProbeConfig struct {
	Enabled             bool           `json:"enabled"`
	IntervalMS          int            `json:"interval_ms"`
	StartupDelayMS      int            `json:"startup_delay_ms"`
	TimeoutMS           int            `json:"timeout_ms"`
	TargetFamilies      []string       `json:"target_families,omitempty"`
	EndpointCandidates  []string       `json:"endpoint_candidates"`
	ImageInput          map[string]any `json:"image_input,omitempty"`
	ResponseStructure   map[string]any `json:"response_structure,omitempty"`
	IdentityConsistency map[string]any `json:"identity_consistency,omitempty"`
	KnowledgeCutoff     map[string]any `json:"knowledge_cutoff,omitempty"`
	LongContext         map[string]any `json:"long_context,omitempty"`
	Raw                 map[string]any `json:"-"`
}

type gatewayConfig struct {
	ListenHost                  string            `json:"listen_host"`
	ListenPort                  int               `json:"listen_port"`
	UpstreamBaseURL             string            `json:"upstream_base_url"`
	RequestBodyLimitBytes       int64             `json:"request_body_limit_bytes"`
	Endpoints                   []string          `json:"endpoints"`
	InterceptRuleMode           string            `json:"intercept_rule_mode"`
	ReasoningMatchMode          string            `json:"reasoning_match_mode"`
	ReasoningEquals             []int             `json:"reasoning_equals"`
	ContinuationMarkerText      string            `json:"continuation_marker_text"`
	InterceptStreaming          bool              `json:"intercept_streaming"`
	InterceptNonStreaming       bool              `json:"intercept_non_streaming"`
	NonStreamStatusCode         int               `json:"non_stream_status_code"`
	GuardRetryAttempts          int               `json:"guard_retry_attempts"`
	RetryUpstreamCapacityErrors bool              `json:"retry_upstream_capacity_errors"`
	StreamAction                string            `json:"stream_action"`
	LogMatch                    bool              `json:"log_match"`
	HealthPath                  string            `json:"health_path"`
	ActiveProbe                 activeProbeConfig `json:"active_probe"`
}

func defaultGatewayConfig() gatewayConfig {
	return gatewayConfig{
		ListenHost:                  defaultListenHost,
		ListenPort:                  defaultListenPort,
		RequestBodyLimitBytes:       defaultRequestBodyLimitBytes,
		Endpoints:                   append([]string{}, defaultEndpoints...),
		InterceptRuleMode:           defaultInterceptRuleMode,
		ReasoningMatchMode:          defaultReasoningMatchMode,
		ReasoningEquals:             append([]int{}, defaultReasoningEquals...),
		ContinuationMarkerText:      defaultContinuationMarkerText,
		InterceptStreaming:          true,
		InterceptNonStreaming:       true,
		NonStreamStatusCode:         502,
		GuardRetryAttempts:          5,
		RetryUpstreamCapacityErrors: true,
		StreamAction:                defaultStreamAction,
		LogMatch:                    true,
		HealthPath:                  defaultHealthPath,
		ActiveProbe: activeProbeConfig{
			Enabled:             false,
			IntervalMS:          900000,
			StartupDelayMS:      60000,
			TimeoutMS:           120000,
			EndpointCandidates:  []string{"/responses", "/v1/responses"},
			ImageInput:          map[string]any{"enabled": true},
			ResponseStructure:   map[string]any{"enabled": false, "repeat_count": 2},
			IdentityConsistency: map[string]any{"enabled": false, "repeat_count": 2},
			KnowledgeCutoff:     map[string]any{"enabled": false, "max_questions": 3},
			LongContext:         map[string]any{"enabled": true, "target_input_tokens": 460000},
		},
	}
}

func loadGatewayConfig(configPath string) (gatewayConfig, error) {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return gatewayConfig{}, err
	}

	config := defaultGatewayConfig()
	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		return gatewayConfig{}, err
	}

	if value := normalizeString(raw["listen_host"], config.ListenHost); value != "" {
		config.ListenHost = value
	}
	if value, err := parseOptionalInt(raw["listen_port"], config.ListenPort); err == nil {
		config.ListenPort = value
	} else {
		return gatewayConfig{}, fmt.Errorf("invalid listen_port: %w", err)
	}
	config.UpstreamBaseURL = normalizeString(raw["upstream_base_url"], config.UpstreamBaseURL)
	if config.UpstreamBaseURL == "" {
		return gatewayConfig{}, errors.New("配置缺少 upstream_base_url")
	}

	if value, err := parseOptionalInt64(raw["request_body_limit_bytes"], defaultRequestBodyLimitBytes); err == nil {
		if value <= 0 {
			config.RequestBodyLimitBytes = defaultRequestBodyLimitBytes
		} else if value == legacyRequestBodyLimitBytes {
			config.RequestBodyLimitBytes = defaultRequestBodyLimitBytes
		} else {
			config.RequestBodyLimitBytes = value
		}
	} else {
		return gatewayConfig{}, fmt.Errorf("invalid request_body_limit_bytes: %w", err)
	}

	config.Endpoints = normalizeStringList(raw["endpoints"], defaultEndpoints)
	if len(config.Endpoints) == 0 {
		config.Endpoints = append([]string{}, defaultEndpoints...)
	}

	config.InterceptRuleMode = normalizeInterceptRuleMode(anyToString(raw["intercept_rule_mode"]), config.InterceptRuleMode)
	config.ReasoningMatchMode = normalizeReasoningMatchMode(anyToString(raw["reasoning_match_mode"]), config.ReasoningMatchMode)
	config.ReasoningEquals = normalizeIntegerList(raw["reasoning_equals"], defaultReasoningEquals)
	if len(config.ReasoningEquals) == 0 {
		return gatewayConfig{}, errors.New("reasoning_equals 不能为空")
	}

	config.ContinuationMarkerText = normalizeContinuationMarkerText(anyToString(raw["continuation_marker_text"]), defaultContinuationMarkerText)
	config.InterceptStreaming = optionalBool(raw["intercept_streaming"], true)
	config.InterceptNonStreaming = optionalBool(raw["intercept_non_streaming"], true)
	if !config.InterceptStreaming && !config.InterceptNonStreaming {
		return gatewayConfig{}, errors.New("流式与非流式至少选择一个拦截目标")
	}

	if value, err := parseOptionalInt(raw["non_stream_status_code"], 502); err == nil {
		config.NonStreamStatusCode = value
	} else {
		return gatewayConfig{}, fmt.Errorf("invalid non_stream_status_code: %w", err)
	}
	if config.NonStreamStatusCode < 100 || config.NonStreamStatusCode > 599 {
		return gatewayConfig{}, errors.New("non_stream_status_code 必须是 100-599 的整数")
	}

	if value, err := parseOptionalInt(raw["guard_retry_attempts"], 5); err == nil {
		if value < 0 {
			return gatewayConfig{}, errors.New("guard_retry_attempts 不能小于 0")
		}
		config.GuardRetryAttempts = value
	} else {
		return gatewayConfig{}, fmt.Errorf("invalid guard_retry_attempts: %w", err)
	}

	config.RetryUpstreamCapacityErrors = optionalBool(raw["retry_upstream_capacity_errors"], true)
	config.StreamAction = normalizeStreamAction(anyToString(raw["stream_action"]), config.StreamAction)
	config.LogMatch = optionalBool(raw["log_match"], true)
	config.HealthPath = normalizePath(normalizeString(raw["health_path"], defaultHealthPath))
	config.ActiveProbe = normalizeActiveProbeConfig(raw["active_probe"], config.ActiveProbe)
	return config, nil
}

func writeGatewayConfig(configPath string, config gatewayConfig) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(configPath, body, 0o644)
}

func buildEditableConfig(current gatewayConfig, payload map[string]any) (gatewayConfig, error) {
	next := current
	next.ReasoningEquals = normalizeIntegerList(payload["reasoning_equals"], current.ReasoningEquals)
	if len(next.ReasoningEquals) == 0 {
		return gatewayConfig{}, errors.New("reasoning_equals 不能为空")
	}

	next.InterceptRuleMode = normalizeInterceptRuleMode(anyToStringOr(payload, "intercept_rule_mode", current.InterceptRuleMode), current.InterceptRuleMode)
	next.ReasoningMatchMode = normalizeReasoningMatchMode(anyToStringOr(payload, "reasoning_match_mode", current.ReasoningMatchMode), current.ReasoningMatchMode)
	next.Endpoints = normalizeStringList(anyOr(payload, "endpoints", current.Endpoints), current.Endpoints)
	if len(next.Endpoints) == 0 {
		return gatewayConfig{}, errors.New("endpoints 不能为空")
	}

	var err error
	if _, ok := payload["non_stream_status_code"]; ok {
		next.NonStreamStatusCode, err = parseOptionalInt(payload["non_stream_status_code"], current.NonStreamStatusCode)
		if err != nil {
			return gatewayConfig{}, fmt.Errorf("non_stream_status_code 必须是整数: %w", err)
		}
	}
	if next.NonStreamStatusCode < 100 || next.NonStreamStatusCode > 599 {
		return gatewayConfig{}, errors.New("non_stream_status_code 必须是 100-599 的整数")
	}

	if _, ok := payload["intercept_streaming"]; ok {
		next.InterceptStreaming = optionalBool(payload["intercept_streaming"], current.InterceptStreaming)
	}
	if _, ok := payload["intercept_non_streaming"]; ok {
		next.InterceptNonStreaming = optionalBool(payload["intercept_non_streaming"], current.InterceptNonStreaming)
	}
	if !next.InterceptStreaming && !next.InterceptNonStreaming {
		return gatewayConfig{}, errors.New("流式与非流式至少选择一个拦截目标")
	}

	if _, ok := payload["guard_retry_attempts"]; ok {
		next.GuardRetryAttempts, err = parseOptionalInt(payload["guard_retry_attempts"], current.GuardRetryAttempts)
		if err != nil || next.GuardRetryAttempts < 0 {
			return gatewayConfig{}, errors.New("guard_retry_attempts 必须是大于等于 0 的整数")
		}
	}

	if _, ok := payload["retry_upstream_capacity_errors"]; ok {
		next.RetryUpstreamCapacityErrors = optionalBool(payload["retry_upstream_capacity_errors"], current.RetryUpstreamCapacityErrors)
	}
	if _, ok := payload["stream_action"]; ok {
		next.StreamAction = normalizeStreamAction(anyToString(payload["stream_action"]), current.StreamAction)
	}
	if _, ok := payload["continuation_marker_text"]; ok {
		next.ContinuationMarkerText = normalizeContinuationMarkerText(anyToString(payload["continuation_marker_text"]), defaultContinuationMarkerText)
	}
	if _, ok := payload["log_match"]; ok {
		next.LogMatch = optionalBool(payload["log_match"], current.LogMatch)
	}
	if _, ok := payload["active_probe"]; ok {
		activeProbePayload, _ := payload["active_probe"].(map[string]any)
		requestedActiveProbeEnabled := current.ActiveProbe.Enabled
		if activeProbePayload != nil {
			if _, hasEnabled := activeProbePayload["enabled"]; hasEnabled {
				requestedActiveProbeEnabled = optionalBool(activeProbePayload["enabled"], current.ActiveProbe.Enabled)
			}
		}
		next.ActiveProbe = normalizeActiveProbeConfig(payload["active_probe"], current.ActiveProbe)
		if requestedActiveProbeEnabled && len(next.ActiveProbe.TargetFamilies) == 0 {
			return gatewayConfig{}, errors.New("开启自动探测前，至少选择一个探测目标模型")
		}
	}
	return next, nil
}

func normalizeActiveProbeConfig(raw any, fallback activeProbeConfig) activeProbeConfig {
	next := fallback
	if raw == nil {
		return next
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		return next
	}
	requestedEnabled := optionalBool(payload["enabled"], fallback.Enabled)
	next.IntervalMS, _ = parseOptionalInt(payload["interval_ms"], fallback.IntervalMS)
	next.StartupDelayMS, _ = parseOptionalInt(payload["startup_delay_ms"], fallback.StartupDelayMS)
	next.TimeoutMS, _ = parseOptionalInt(payload["timeout_ms"], fallback.TimeoutMS)
	next.TargetFamilies = normalizeActiveProbeTargetFamilies(payload["target_families"], fallback.TargetFamilies)
	next.EndpointCandidates = normalizeStringList(payload["endpoint_candidates"], fallback.EndpointCandidates)
	next.ImageInput = normalizeMap(payload["image_input"], fallback.ImageInput)
	next.ResponseStructure = normalizeMap(payload["response_structure"], fallback.ResponseStructure)
	next.IdentityConsistency = normalizeMap(payload["identity_consistency"], fallback.IdentityConsistency)
	next.KnowledgeCutoff = normalizeMap(payload["knowledge_cutoff"], fallback.KnowledgeCutoff)
	next.LongContext = normalizeMap(payload["long_context"], fallback.LongContext)
	next.Enabled = requestedEnabled && len(next.TargetFamilies) > 0
	next.Raw = payload
	return next
}

func normalizeActiveProbeTargetFamilies(raw any, fallback []string) []string {
	var source []string
	switch value := raw.(type) {
	case nil:
		source = append([]string{}, fallback...)
	case []string:
		source = value
	case []any:
		for _, item := range value {
			if text := strings.Trim(strings.TrimSpace(anyToString(item)), "/"); text != "" {
				source = append(source, text)
			}
		}
	default:
		if text := strings.Trim(strings.TrimSpace(anyToString(value)), "/"); text != "" {
			source = append(source, text)
		}
	}

	result := make([]string, 0, len(source))
	for _, item := range source {
		family := normalizeModelFamily(strings.Trim(strings.TrimSpace(item), "/"))
		if family == "" || family == "unknown" || slices.Contains(result, family) {
			continue
		}
		result = append(result, family)
	}
	return result
}

func normalizeInterceptRuleMode(raw string, fallback string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch value {
	case "", "continuation_recovery":
		return fallback
	case defaultInterceptRuleMode, finalOnlyRuleMode:
		return value
	default:
		return fallback
	}
}

func normalizeReasoningMatchMode(raw string, fallback string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch value {
	case "", defaultReasoningMatchMode, "manual":
		if value == "" {
			return fallback
		}
		return value
	default:
		return fallback
	}
}

func normalizeStreamAction(raw string, fallback string) string {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch value {
	case "", "strict_502", "disconnect", "continuation_recovery":
		if value == "" {
			return fallback
		}
		return value
	default:
		return fallback
	}
}

func normalizeContinuationMarkerText(raw string, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return fallback
	}
	return value
}

func normalizeStringList(raw any, fallback []string) []string {
	var source []string
	switch value := raw.(type) {
	case nil:
		source = append([]string{}, fallback...)
	case []string:
		source = value
	case []any:
		for _, item := range value {
			text := strings.TrimSpace(anyToString(item))
			if text != "" {
				source = append(source, normalizePath(text))
			}
		}
	default:
		text := strings.TrimSpace(anyToString(value))
		if text != "" {
			source = append(source, normalizePath(text))
		}
	}

	result := make([]string, 0, len(source))
	for _, item := range source {
		item = normalizePath(item)
		if item == "" || slices.Contains(result, item) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func normalizeIntegerList(raw any, fallback []int) []int {
	var values []int
	switch value := raw.(type) {
	case nil:
		values = append(values, fallback...)
	case []int:
		values = append(values, value...)
	case []any:
		for _, item := range value {
			if parsed, err := parseInt(anyToString(item)); err == nil {
				values = append(values, parsed)
			}
		}
	default:
		if parsed, err := parseInt(anyToString(value)); err == nil {
			values = append(values, parsed)
		}
	}

	result := make([]int, 0, len(values))
	seen := map[int]bool{}
	for _, item := range values {
		if seen[item] {
			continue
		}
		seen[item] = true
		result = append(result, item)
	}
	return result
}

func normalizeString(raw any, fallback string) string {
	value := strings.TrimSpace(anyToString(raw))
	if value == "" {
		return fallback
	}
	return value
}

func normalizeMap(raw any, fallback map[string]any) map[string]any {
	if payload, ok := raw.(map[string]any); ok {
		return payload
	}
	if fallback == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(fallback))
	for key, value := range fallback {
		cloned[key] = value
	}
	return cloned
}

func normalizePath(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	value = strings.ReplaceAll(value, "\\", "/")
	for strings.Contains(value, "//") {
		value = strings.ReplaceAll(value, "//", "/")
	}
	if len(value) > 1 && strings.HasSuffix(value, "/") {
		value = strings.TrimSuffix(value, "/")
	}
	return value
}

func anyToString(raw any) string {
	switch value := raw.(type) {
	case nil:
		return ""
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	case json.Number:
		return value.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

func anyToStringOr(payload map[string]any, key string, fallback string) string {
	if value, ok := payload[key]; ok {
		return anyToString(value)
	}
	return fallback
}

func anyOr(payload map[string]any, key string, fallback any) any {
	if value, ok := payload[key]; ok {
		return value
	}
	return fallback
}

func optionalBool(raw any, fallback bool) bool {
	switch value := raw.(type) {
	case nil:
		return fallback
	case bool:
		return value
	case string:
		switch strings.TrimSpace(strings.ToLower(value)) {
		case "true":
			return true
		case "false":
			return false
		}
	}
	return fallback
}

func parseOptionalInt(raw any, fallback int) (int, error) {
	if raw == nil {
		return fallback, nil
	}
	switch value := raw.(type) {
	case float64:
		return int(value), nil
	case int:
		return value, nil
	}
	return parseInt(anyToString(raw))
}

func parseOptionalInt64(raw any, fallback int64) (int64, error) {
	if raw == nil {
		return fallback, nil
	}
	switch value := raw.(type) {
	case float64:
		return int64(value), nil
	case int:
		return int64(value), nil
	case int64:
		return value, nil
	}
	return parseInt64(anyToString(raw))
}

func parseInt(raw string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(raw))
}

func parseInt64(raw string) (int64, error) {
	return strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
}
