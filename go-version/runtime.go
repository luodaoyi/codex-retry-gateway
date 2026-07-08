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
	mu                               sync.Mutex
	StartedAt                        string         `json:"started_at"`
	NextLogSeq                       int            `json:"-"`
	LogEntries                       []logEntry     `json:"-"`
	TotalProxyRequestCount           int            `json:"total_proxy_request_count"`
	InspectedResponseCount           int            `json:"inspected_response_count"`
	BypassedProxyRequestCount        int            `json:"bypassed_proxy_request_count"`
	BypassedProxyPathCounts          map[string]int `json:"bypassed_proxy_path_counts"`
	FailedProxyRequestCount          int            `json:"failed_proxy_request_count"`
	ActiveProxyRequestCount          int            `json:"active_proxy_request_count"`
	ActiveProxyPathCounts            map[string]int `json:"active_proxy_path_counts"`
	MatchedResponseCount             int            `json:"matched_response_count"`
	MatchedStreamingCount            int            `json:"matched_streaming_count"`
	MatchedNonStreamingCount         int            `json:"matched_non_streaming_count"`
	BlockedResponseCount             int            `json:"blocked_response_count"`
	BlockedStreamingCount            int            `json:"blocked_streaming_count"`
	BlockedNonStreamingCount         int            `json:"blocked_non_streaming_count"`
	ContinuationRecoveryCount        int            `json:"continuation_recovery_count"`
	ContinuationRecoverySuccessCount int            `json:"continuation_recovery_success_count"`
	ObservedReasoningCounts          map[string]int `json:"observed_reasoning_counts"`
	LocalModelCounts                 map[string]int `json:"local_model_counts"`
	UpstreamModelCounts              map[string]int `json:"upstream_model_counts"`
	StreamModelCounts                map[string]int `json:"stream_model_counts"`
	ModelConsistency                 modelConsistencyCounters
	ModelFamilyAnomalies             modelFamilyAnomalyCounters
	SingleRequestAnomalies           singleRequestAnomalyCounters
	FamilyBreakdown                  map[string]*familyBreakdownEntry
	SuspiciousModelSamples           []suspiciousModelSample
}

type reasoningSample struct {
	SampleID                   string         `json:"sample_id,omitempty"`
	GatewayRequestID           string         `json:"gateway_request_id,omitempty"`
	AttemptID                  string         `json:"attempt_id,omitempty"`
	ID                         string         `json:"id"`
	RecordedAt                 string         `json:"recorded_at"`
	TS                         string         `json:"ts,omitempty"`
	DateKey                    string         `json:"date_key,omitempty"`
	Path                       string         `json:"path,omitempty"`
	Pathname                   string         `json:"pathname"`
	Method                     string         `json:"method"`
	RequestKind                string         `json:"request_kind"`
	RequestModel               string         `json:"request_model,omitempty"`
	RequestModelFamily         string         `json:"request_model_family,omitempty"`
	EffectiveLocalModel        string         `json:"effective_local_model,omitempty"`
	EffectiveLocalModelFamily  string         `json:"effective_local_model_family,omitempty"`
	UpstreamModel              string         `json:"upstream_model,omitempty"`
	StreamModel                string         `json:"stream_model,omitempty"`
	FinalResponseModel         string         `json:"final_response_model,omitempty"`
	SystemFingerprint          string         `json:"system_fingerprint,omitempty"`
	ServiceTier                string         `json:"service_tier,omitempty"`
	RequestReasoningEffort     string         `json:"request_reasoning_effort,omitempty"`
	RequestPayloadExcerpt      string         `json:"request_payload_excerpt,omitempty"`
	RequestStartedAt           string         `json:"request_started_at,omitempty"`
	RequestFinishedAt          string         `json:"request_finished_at,omitempty"`
	DurationTotalMS            *int           `json:"duration_total_ms,omitempty"`
	InputTokens                *int           `json:"input_tokens,omitempty"`
	Streaming                  bool           `json:"streaming"`
	ReasoningTokens            *int           `json:"reasoning_tokens,omitempty"`
	OutputTokens               *int           `json:"output_tokens,omitempty"`
	TotalTokens                *int           `json:"total_tokens,omitempty"`
	OutputTPS                  any            `json:"output_tps,omitempty"`
	ReasoningAdjustedTPS       any            `json:"reasoning_adjusted_tps,omitempty"`
	TimeNormalizationDeviation any            `json:"time_normalization_deviation,omitempty"`
	MatchedCurrentRule         bool           `json:"matched_current_rule"`
	FinalAction                string         `json:"final_action"`
	UpstreamHTTPStatus         *int           `json:"upstream_http_status,omitempty"`
	ClientHTTPStatus           *int           `json:"client_http_status,omitempty"`
	BlockedByGateway           bool           `json:"blocked_by_gateway"`
	UpstreamStreamTerminated   bool           `json:"upstream_stream_terminated"`
	InternalRetryAttemptIndex  int            `json:"internal_retry_attempt_index"`
	InternalRetryRemaining     *int           `json:"internal_retry_remaining,omitempty"`
	InterceptExemptReason      string         `json:"intercept_exempt_reason,omitempty"`
	FinalAnswerOnly            bool           `json:"final_answer_only"`
	CommentaryObserved         bool           `json:"commentary_observed"`
	CommentaryNotObserved      bool           `json:"commentary_not_observed"`
	HasCommentary              bool           `json:"has_commentary"`
	HasFinalAnswer             bool           `json:"has_final_answer"`
	HasToolCall                bool           `json:"has_tool_call"`
	HasReasoningItem           bool           `json:"has_reasoning_item"`
	RequestSummary             map[string]any `json:"request_summary,omitempty"`
	FailureSummary             map[string]any `json:"failure_summary,omitempty"`
	Structure                  map[string]any `json:"structure,omitempty"`
	Extra                      map[string]any `json:"extra,omitempty"`
}

type reasoningExportJob struct {
	JobID         string   `json:"job_id"`
	Status        string   `json:"status"`
	Format        string   `json:"format"`
	DateFrom      string   `json:"date_from,omitempty"`
	DateTo        string   `json:"date_to,omitempty"`
	DateKeys      []string `json:"-"`
	TotalDays     int      `json:"total_days"`
	ProcessedDays int      `json:"processed_days"`
	SampleCount   int      `json:"sample_count"`
	CreatedAt     string   `json:"created_at"`
	StartedAt     string   `json:"started_at,omitempty"`
	FinishedAt    string   `json:"finished_at,omitempty"`
	OutputPath    string   `json:"output_path,omitempty"`
	ErrorMessage  string   `json:"error_message,omitempty"`
}

type reasoningBehaviorState struct {
	StartedAt             string
	RecentSamples         []reasoningSample
	DailyBuffers          map[string][]reasoningSample
	FlushTimers           map[string]*time.Timer
	LastFlushAt           string
	LastFlushError        string
	NextExportJobSequence int
	ExportJobs            map[string]*reasoningExportJob
}

type loggerFunc func(message string)

type appRuntime struct {
	Config            gatewayConfig
	ConfigPath        string
	LogPath           string
	Paths             gatewayPaths
	Logger            loggerFunc
	Monitor           *monitor
	ReasoningMu       sync.Mutex
	ReasoningBehavior *reasoningBehaviorState
	HistoricalImports *historicalImportState
	ActiveProbe       *activeProbeMonitor
	ProbeStartupTimer *time.Timer
	ProbeTimer        *time.Ticker
	RequestSeq        int
}

func newMonitor() *monitor {
	return &monitor{
		StartedAt:               time.Now().Format(time.RFC3339),
		NextLogSeq:              1,
		LogEntries:              []logEntry{},
		BypassedProxyPathCounts: map[string]int{},
		ActiveProxyPathCounts:   map[string]int{},
		ObservedReasoningCounts: map[string]int{},
		LocalModelCounts:        map[string]int{},
		UpstreamModelCounts:     map[string]int{},
		StreamModelCounts:       map[string]int{},
		FamilyBreakdown:         createTrackedFamilyBreakdown(),
		SuspiciousModelSamples:  []suspiciousModelSample{},
	}
}

func newReasoningBehaviorState() *reasoningBehaviorState {
	return &reasoningBehaviorState{
		StartedAt:             time.Now().Format(time.RFC3339),
		RecentSamples:         []reasoningSample{},
		DailyBuffers:          map[string][]reasoningSample{},
		FlushTimers:           map[string]*time.Timer{},
		NextExportJobSequence: 1,
		ExportJobs:            map[string]*reasoningExportJob{},
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
		"started_at":                          mon.StartedAt,
		"total_proxy_request_count":           mon.TotalProxyRequestCount,
		"inspected_response_count":            mon.InspectedResponseCount,
		"bypassed_proxy_request_count":        mon.BypassedProxyRequestCount,
		"bypassed_proxy_path_counts":          cloneCounter(mon.BypassedProxyPathCounts),
		"failed_proxy_request_count":          mon.FailedProxyRequestCount,
		"active_proxy_request_count":          mon.ActiveProxyRequestCount,
		"active_proxy_path_counts":            cloneCounter(mon.ActiveProxyPathCounts),
		"matched_response_count":              mon.MatchedResponseCount,
		"matched_streaming_count":             mon.MatchedStreamingCount,
		"matched_non_streaming_count":         mon.MatchedNonStreamingCount,
		"blocked_response_count":              mon.BlockedResponseCount,
		"blocked_streaming_count":             mon.BlockedStreamingCount,
		"blocked_non_streaming_count":         mon.BlockedNonStreamingCount,
		"continuation_recovery_count":         continuationCount,
		"continuation_recovery_success_count": continuationSuccess,
		"continuation_recovery_success_ratio": ratio,
		"reasoning_516_count":                 reasoning516Count,
		"reasoning_516_ratio":                 reasoningRatio,
		"observed_reasoning_counts":           cloneCounter(mon.ObservedReasoningCounts),
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
	if sample.RecordedAt == "" {
		sample.RecordedAt = time.Now().Format(time.RFC3339)
	}
	if sample.TS == "" {
		sample.TS = sample.RecordedAt
	}
	if sample.DateKey == "" {
		if parsed, err := time.Parse(time.RFC3339, sample.TS); err == nil {
			sample.DateKey = toLocalDateKey(parsed)
		} else {
			sample.DateKey = toLocalDateKey(time.Now())
		}
	}
	if sample.Path == "" {
		sample.Path = sample.Pathname
	}
	if sample.SampleID == "" {
		sample.SampleID = sample.ID
	}
	if sample.GatewayRequestID == "" {
		sample.GatewayRequestID = sample.ID
	}
	normalized := clonePlainSample(sample)

	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	runtime.ReasoningBehavior.RecentSamples = append([]reasoningSample{normalized}, runtime.ReasoningBehavior.RecentSamples...)
	if len(runtime.ReasoningBehavior.RecentSamples) > 500 {
		runtime.ReasoningBehavior.RecentSamples = runtime.ReasoningBehavior.RecentSamples[:500]
	}
	runtime.ReasoningBehavior.DailyBuffers[normalized.DateKey] = append(runtime.ReasoningBehavior.DailyBuffers[normalized.DateKey], normalized)
	runtime.scheduleReasoningBehaviorFlushLocked(normalized.DateKey)
}

func buildReasoningBehaviorRuntimeSnapshot(runtime *appRuntime) map[string]any {
	runtime.ReasoningMu.Lock()
	samples := append([]reasoningSample{}, runtime.ReasoningBehavior.RecentSamples...)
	runtime.ReasoningMu.Unlock()
	snapshot := buildReasoningBehaviorSnapshotLocked(runtime, samples, 50)
	for key, value := range buildReasoningBehaviorMetadata(runtime) {
		snapshot[key] = value
	}
	return snapshot
}

func buildReasoningBehaviorSnapshotLocked(runtime *appRuntime, samples []reasoningSample, recentLimit int) map[string]any {
	recent := append([]reasoningSample{}, samples...)
	if recentLimit > 0 && len(recent) > recentLimit {
		recent = recent[:recentLimit]
	}
	matchedCount := 0
	blockedCount := 0
	for _, sample := range samples {
		if sample.MatchedCurrentRule {
			matchedCount++
		}
		if sample.BlockedByGateway {
			blockedCount++
		}
	}
	return map[string]any{
		"schema_version":            2,
		"sample_count":              len(samples),
		"matched_count":             matchedCount,
		"blocked_count":             blockedCount,
		"recent_samples":            recent,
		"tracked_range_hint":        "runtime",
		"commentary_observed_ratio": 0,
	}
}

func jsonMarshal(v any) string {
	body, _ := json.Marshal(v)
	return string(body)
}

func buildReasoningBehaviorMetadata(runtime *appRuntime) map[string]any {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	if runtime.ReasoningBehavior == nil {
		return map[string]any{
			"schema_version":             2,
			"analytics_ready":            false,
			"analytics_started_at":       nil,
			"analytics_state_root":       runtime.Paths.AnalyticsRoot,
			"analytics_last_flush_at":    nil,
			"analytics_last_flush_error": nil,
		}
	}

	var lastFlushAt any
	if runtime.ReasoningBehavior.LastFlushAt != "" {
		lastFlushAt = runtime.ReasoningBehavior.LastFlushAt
	}
	var lastFlushError any
	if runtime.ReasoningBehavior.LastFlushError != "" {
		lastFlushError = runtime.ReasoningBehavior.LastFlushError
	}

	return map[string]any{
		"schema_version":             2,
		"analytics_ready":            true,
		"analytics_started_at":       runtime.ReasoningBehavior.StartedAt,
		"analytics_state_root":       runtime.Paths.AnalyticsRoot,
		"analytics_last_flush_at":    lastFlushAt,
		"analytics_last_flush_error": lastFlushError,
	}
}

func incrementReasoningCount(counter map[string]int, reasoning *int) {
	if reasoning == nil {
		return
	}
	counter[fmt.Sprintf("%d", *reasoning)]++
}

func recordInspectedResponse(mon *monitor, reasoning *int, matched bool, streamKind string) {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	mon.InspectedResponseCount++
	incrementReasoningCount(mon.ObservedReasoningCounts, reasoning)
	if matched {
		mon.MatchedResponseCount++
		switch streamKind {
		case "stream":
			mon.MatchedStreamingCount++
		case "non-stream":
			mon.MatchedNonStreamingCount++
		}
	}
}

func recordBlockedResponse(mon *monitor, streamKind string) {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	mon.BlockedResponseCount++
	switch streamKind {
	case "stream":
		mon.BlockedStreamingCount++
	case "non-stream":
		mon.BlockedNonStreamingCount++
	}
}

func recordContinuationRecoveryAttempt(mon *monitor, tracking *requestTracking) {
	mon.mu.Lock()
	defer mon.mu.Unlock()
	mon.ContinuationRecoveryCount++
	if tracking != nil {
		tracking.ContinuationRecoveryAttempted = true
	}
}

func recordContinuationRecoverySuccess(mon *monitor, tracking *requestTracking) {
	if tracking == nil || !tracking.ContinuationRecoveryAttempted || tracking.ContinuationRecoverySuccessRecorded {
		return
	}
	mon.mu.Lock()
	defer mon.mu.Unlock()
	if tracking.ContinuationRecoverySuccessRecorded {
		return
	}
	tracking.ContinuationRecoverySuccessRecorded = true
	mon.ContinuationRecoverySuccessCount++
}
