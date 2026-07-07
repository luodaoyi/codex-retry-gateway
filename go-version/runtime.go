package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type logEntry struct {
	Seq     int    `json:"seq"`
	At      string `json:"at"`
	Message string `json:"message"`
}

type monitor struct {
	mu                                sync.Mutex
	StartedAt                         string         `json:"started_at"`
	NextLogSeq                        int            `json:"-"`
	LogEntries                        []logEntry     `json:"-"`
	TotalProxyRequestCount            int            `json:"total_proxy_request_count"`
	InspectedResponseCount            int            `json:"inspected_response_count"`
	BypassedProxyRequestCount         int            `json:"bypassed_proxy_request_count"`
	BypassedProxyPathCounts           map[string]int `json:"bypassed_proxy_path_counts"`
	FailedProxyRequestCount           int            `json:"failed_proxy_request_count"`
	ActiveProxyRequestCount           int            `json:"active_proxy_request_count"`
	ActiveProxyPathCounts             map[string]int `json:"active_proxy_path_counts"`
	MatchedResponseCount              int            `json:"matched_response_count"`
	MatchedStreamingCount             int            `json:"matched_streaming_count"`
	MatchedNonStreamingCount          int            `json:"matched_non_streaming_count"`
	BlockedResponseCount              int            `json:"blocked_response_count"`
	BlockedStreamingCount             int            `json:"blocked_streaming_count"`
	BlockedNonStreamingCount          int            `json:"blocked_non_streaming_count"`
	ContinuationRecoveryCount         int            `json:"continuation_recovery_count"`
	ContinuationRecoverySuccessCount  int            `json:"continuation_recovery_success_count"`
	ObservedReasoningCounts           map[string]int `json:"observed_reasoning_counts"`
}

type reasoningSample struct {
	ID                  string                 `json:"id"`
	RecordedAt          string                 `json:"recorded_at"`
	Pathname            string                 `json:"pathname"`
	Method              string                 `json:"method"`
	RequestKind         string                 `json:"request_kind"`
	RequestModel        string                 `json:"request_model,omitempty"`
	RequestReasoningEffort string              `json:"request_reasoning_effort,omitempty"`
	Streaming           bool                   `json:"streaming"`
	ReasoningTokens     *int                   `json:"reasoning_tokens,omitempty"`
	MatchedCurrentRule  bool                   `json:"matched_current_rule"`
	FinalAction         string                 `json:"final_action"`
	ClientHTTPStatus    *int                   `json:"client_http_status,omitempty"`
	BlockedByGateway    bool                   `json:"blocked_by_gateway"`
	InterceptExemptReason string               `json:"intercept_exempt_reason,omitempty"`
	RequestSummary      map[string]any         `json:"request_summary,omitempty"`
	FailureSummary      map[string]any         `json:"failure_summary,omitempty"`
	Structure           map[string]any         `json:"structure,omitempty"`
	Extra               map[string]any         `json:"extra,omitempty"`
}

type loggerFunc func(message string)

type appRuntime struct {
	Config           gatewayConfig
	ConfigPath       string
	LogPath          string
	Paths            gatewayPaths
	Logger           loggerFunc
	Monitor          *monitor
	ReasoningMu      sync.Mutex
	ReasoningSamples []reasoningSample
	RequestSeq       int
}

func newMonitor() *monitor {
	return &monitor{
		StartedAt:                time.Now().Format(time.RFC3339),
		NextLogSeq:               1,
		LogEntries:               []logEntry{},
		BypassedProxyPathCounts:  map[string]int{},
		ActiveProxyPathCounts:    map[string]int{},
		ObservedReasoningCounts:  map[string]int{},
	}
}

func createLogger(logPath string, mon *monitor) (loggerFunc, error) {
	var file *os.File
	var err error
	if strings.TrimSpace(logPath) != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
	}

	return func(message string) {
		entry := recordLogEntry(mon, message)
		line := fmt.Sprintf("%s %s\n", entry.At, entry.Message)
		if file != nil {
			_, _ = file.WriteString(line)
		}
		_, _ = os.Stdout.WriteString(line)
	}, nil
}

func recordLogEntry(mon *monitor, message string) logEntry {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	entry := logEntry{
		Seq:     mon.NextLogSeq,
		At:      time.Now().Format(time.RFC3339),
		Message: message,
	}
	mon.NextLogSeq++
	mon.LogEntries = append(mon.LogEntries, entry)
	if len(mon.LogEntries) > 2000 {
		mon.LogEntries = append([]logEntry{}, mon.LogEntries[len(mon.LogEntries)-2000:]...)
	}
	return entry
}

func buildLogsSnapshot(mon *monitor, sinceSeq *int) map[string]any {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	entries := make([]logEntry, 0, len(mon.LogEntries))
	for _, entry := range mon.LogEntries {
		if sinceSeq != nil && entry.Seq <= *sinceSeq {
			continue
		}
		entries = append(entries, entry)
	}
	return map[string]any{
		"total_entries": len(mon.LogEntries),
		"latest_seq":    mon.NextLogSeq - 1,
		"entries":       entries,
	}
}

func buildMetricsSnapshot(mon *monitor) map[string]any {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	reasoning516Count := mon.ObservedReasoningCounts["516"]
	continuationCount := mon.ContinuationRecoveryCount
	continuationSuccess := mon.ContinuationRecoverySuccessCount
	ratio := 0.0
	if continuationCount > 0 {
		ratio = float64(continuationSuccess) / float64(continuationCount)
	}
	reasoningRatio := 0.0
	if mon.InspectedResponseCount > 0 {
		reasoningRatio = float64(reasoning516Count) / float64(mon.InspectedResponseCount)
	}
	return map[string]any{
		"started_at":                           mon.StartedAt,
		"total_proxy_request_count":            mon.TotalProxyRequestCount,
		"inspected_response_count":             mon.InspectedResponseCount,
		"bypassed_proxy_request_count":         mon.BypassedProxyRequestCount,
		"bypassed_proxy_path_counts":           cloneCounter(mon.BypassedProxyPathCounts),
		"failed_proxy_request_count":           mon.FailedProxyRequestCount,
		"active_proxy_request_count":           mon.ActiveProxyRequestCount,
		"active_proxy_path_counts":             cloneCounter(mon.ActiveProxyPathCounts),
		"matched_response_count":               mon.MatchedResponseCount,
		"matched_streaming_count":              mon.MatchedStreamingCount,
		"matched_non_streaming_count":          mon.MatchedNonStreamingCount,
		"blocked_response_count":               mon.BlockedResponseCount,
		"blocked_streaming_count":              mon.BlockedStreamingCount,
		"blocked_non_streaming_count":          mon.BlockedNonStreamingCount,
		"continuation_recovery_count":          continuationCount,
		"continuation_recovery_success_count":  continuationSuccess,
		"continuation_recovery_success_ratio":  ratio,
		"reasoning_516_count":                  reasoning516Count,
		"reasoning_516_ratio":                  reasoningRatio,
		"observed_reasoning_counts":            cloneCounter(mon.ObservedReasoningCounts),
	}
}

func cloneCounter(source map[string]int) map[string]int {
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (runtime *appRuntime) nextRequestID() string {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	runtime.RequestSeq++
	return fmt.Sprintf("req-%06d", runtime.RequestSeq)
}

func (runtime *appRuntime) appendReasoningSample(sample reasoningSample) {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	runtime.ReasoningSamples = append(runtime.ReasoningSamples, sample)
	if len(runtime.ReasoningSamples) > 500 {
		runtime.ReasoningSamples = append([]reasoningSample{}, runtime.ReasoningSamples[len(runtime.ReasoningSamples)-500:]...)
	}
}

func buildReasoningBehaviorRuntimeSnapshot(runtime *appRuntime) map[string]any {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	recent := append([]reasoningSample{}, runtime.ReasoningSamples...)
	if len(recent) > 50 {
		recent = recent[len(recent)-50:]
	}
	schemaVersion := 2
	matchedCount := 0
	blockedCount := 0
	for _, sample := range recent {
		if sample.MatchedCurrentRule {
			matchedCount++
		}
		if sample.BlockedByGateway {
			blockedCount++
		}
	}
	return map[string]any{
		"schema_version":       schemaVersion,
		"sample_count":         len(recent),
		"matched_count":        matchedCount,
		"blocked_count":        blockedCount,
		"recent_samples":       recent,
		"tracked_range_hint":   "runtime",
		"commentary_observed_ratio": 0,
	}
}

func jsonMarshal(v any) string {
	body, _ := json.Marshal(v)
	return string(body)
}
