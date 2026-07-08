package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	reasoningBehaviorSchemaVersion           = 2
	reasoningBehaviorRecentSampleLimit       = 500
	reasoningBehaviorMaxInlineRangeDays      = 7
	reasoningBehaviorBackgroundExportMinDays = 32
	reasoningBehaviorExportJobLimit          = 5
	reasoningAnalysisProfileName             = "516_candidate_review_v1"
)

var (
	dateKeyPattern              = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	reasoningDayFileNameRegexp  = regexp.MustCompile(`^reasoning-behavior-(\d{4}-\d{2}-\d{2})\.json$`)
	reasoningAnalysisCoreFields = []string{
		"reasoning_tokens",
		"final_answer_only",
		"commentary_observed",
	}
	reasoningAnalysisFields = []string{
		"reasoning_tokens",
		"final_answer_only",
		"commentary_observed",
		"duration_total_ms",
		"output_tokens",
		"model_family",
		"reasoning_effort",
		"status",
		"retry_status",
		"blocked_status",
	}
)

type reasoningBehaviorDayFile struct {
	Date          string            `json:"date"`
	SchemaVersion int               `json:"schema_version"`
	GeneratedBy   string            `json:"generated_by"`
	Samples       []reasoningSample `json:"samples"`
	DailySummary  map[string]any    `json:"daily_summary"`
}

type reasoningAnalysisFilters struct {
	DateFrom        string   `json:"date_from,omitempty"`
	DateTo          string   `json:"date_to,omitempty"`
	ModelFamily     []string `json:"model_family,omitempty"`
	Model           []string `json:"model,omitempty"`
	ReasoningEffort []string `json:"reasoning_effort,omitempty"`
	Status          string   `json:"status"`
	IncludeRetries  bool     `json:"include_retries"`
	IncludeBlocked  bool     `json:"include_blocked"`
}

type reasoningAnalysisConditions struct {
	ReasoningTokens         []int  `json:"reasoning_tokens"`
	ReasoningTokensMode     string `json:"reasoning_tokens_mode"`
	FinalAnswerOnly         bool   `json:"final_answer_only"`
	CommentaryNotObserved   bool   `json:"commentary_not_observed"`
	TimeNormalizationFactor string `json:"time_normalization_deviation"`
}

type reasoningAnalysisProfile struct {
	Name       string                      `json:"name"`
	DataSource string                      `json:"data_source"`
	Filters    reasoningAnalysisFilters    `json:"filters"`
	Conditions reasoningAnalysisConditions `json:"conditions"`
	Baseline   map[string]any              `json:"baseline"`
}

type groupedSampleSummary struct {
	Key                     string
	Count                   int
	Ratio                   float64
	FinalAnswerOnlyRatio    float64
	CommentaryPresentRatio  float64
	CommentaryObservedRatio float64
	AvgDurationTotalMS      any
	AvgOutputTPS            any
	AvgReasoningAdjustedTPS any
	TopReasoningTokens      []map[string]any
}

func clonePlainSample(sample reasoningSample) reasoningSample {
	body, _ := json.Marshal(sample)
	var cloned reasoningSample
	_ = json.Unmarshal(body, &cloned)
	cloned.Path = firstNonEmpty(cloned.Path, cloned.Pathname)
	cloned.TS = firstNonEmpty(cloned.TS, cloned.RecordedAt)
	cloned.RequestModelFamily = firstNonEmpty(cloned.RequestModelFamily, normalizeModelFamily(cloned.RequestModel))
	cloned.EffectiveLocalModelFamily = firstNonEmpty(cloned.EffectiveLocalModelFamily, cloned.RequestModelFamily)
	cloned.HasCommentary = cloned.HasCommentary || sampleStructureBool(cloned, "has_commentary")
	cloned.HasFinalAnswer = cloned.HasFinalAnswer || sampleStructureBool(cloned, "has_final_answer")
	cloned.HasToolCall = cloned.HasToolCall || sampleStructureBool(cloned, "has_tool_call")
	cloned.HasReasoningItem = cloned.HasReasoningItem || sampleStructureBool(cloned, "has_reasoning_item")
	if !cloned.FinalAnswerOnly {
		cloned.FinalAnswerOnly = cloned.HasFinalAnswer && !cloned.HasCommentary && !cloned.HasToolCall && !cloned.HasReasoningItem
	}
	cloned.CommentaryObserved = cloned.CommentaryObserved || cloned.HasCommentary
	cloned.CommentaryNotObserved = !cloned.CommentaryObserved
	if cloned.SampleID == "" {
		cloned.SampleID = cloned.ID
	}
	if cloned.GatewayRequestID == "" {
		cloned.GatewayRequestID = cloned.ID
	}
	if cloned.RequestPayloadExcerpt != "" {
		cloned.RequestPayloadExcerpt = redactEncryptedContentText(cloned.RequestPayloadExcerpt)
	}
	return cloned
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sampleStructureBool(sample reasoningSample, key string) bool {
	if sample.Structure == nil {
		return false
	}
	value, _ := sample.Structure[key].(bool)
	return value
}

func normalizeModelFamily(modelName string) string {
	value := strings.TrimSpace(strings.ToLower(modelName))
	if value == "" {
		return "unknown"
	}
	switch {
	case strings.HasPrefix(value, "gpt-5.4-mini"):
		return "gpt-5.4-mini"
	case strings.HasPrefix(value, "gpt-5.5-mini"):
		return "gpt-5.5-mini"
	case strings.HasPrefix(value, "gpt-5.4-nano"):
		return "gpt-5.4-nano"
	case strings.HasPrefix(value, "gpt-5.5-nano"):
		return "gpt-5.5-nano"
	case strings.HasPrefix(value, "gpt-5.4"):
		return "gpt-5.4"
	case strings.HasPrefix(value, "gpt-5.5"):
		return "gpt-5.5"
	case strings.Contains(value, "mini"):
		return "mini"
	case strings.Contains(value, "nano"):
		return "nano"
	default:
		return "other"
	}
}

func toLocalDateKey(value time.Time) string {
	return value.Format("2006-01-02")
}

func normalizeDateKeyInput(value string) string {
	trimmed := strings.TrimSpace(value)
	if dateKeyPattern.MatchString(trimmed) {
		return trimmed
	}
	return ""
}

func isDateKeyWithinRange(dateKey string, dateFrom string, dateTo string) bool {
	if dateKey == "" {
		return false
	}
	if dateFrom != "" && dateKey < dateFrom {
		return false
	}
	if dateTo != "" && dateKey > dateTo {
		return false
	}
	return true
}

func countInclusiveDateRangeDays(dateFrom string, dateTo string) (int, bool) {
	normalizedFrom := normalizeDateKeyInput(dateFrom)
	normalizedTo := normalizeDateKeyInput(dateTo)
	if normalizedFrom == "" || normalizedTo == "" {
		return 0, false
	}
	fromTime, err := time.Parse("2006-01-02", normalizedFrom)
	if err != nil {
		return 0, false
	}
	toTime, err := time.Parse("2006-01-02", normalizedTo)
	if err != nil || toTime.Before(fromTime) {
		return 0, false
	}
	return int(toTime.Sub(fromTime).Hours()/24) + 1, true
}

func addDaysToDateKey(dateKey string, days int) string {
	base, err := time.Parse("2006-01-02", dateKey)
	if err != nil {
		return ""
	}
	return base.AddDate(0, 0, days).Format("2006-01-02")
}

func listInclusiveDateKeys(dateFrom string, dateTo string) []string {
	totalDays, ok := countInclusiveDateRangeDays(dateFrom, dateTo)
	if !ok || totalDays <= 0 {
		return []string{}
	}
	keys := make([]string, 0, totalDays)
	for index := 0; index < totalDays; index++ {
		keys = append(keys, addDaysToDateKey(dateFrom, index))
	}
	return keys
}

func buildReasoningRangeDegradePayload(runtime *appRuntime, dateFrom string, dateTo string, maxDays int) map[string]any {
	payload := map[string]any{
		"ok":                    true,
		"date_from":             dateFrom,
		"date_to":               dateTo,
		"degraded":              true,
		"degrade_reason":        "date_range_too_large",
		"max_inline_range_days": maxDays,
		"message":               "时间段过大，已跳过明细读取；请缩小时间段或使用分片/压缩包导出。",
		"summary": map[string]any{
			"total_samples": 0,
			"wording":       "统计结果用于发现候选复盘特征，不代表最终归因。",
		},
		"top_reasoning_tokens":       []any{},
		"output_tps_buckets":         []any{},
		"by_model_family":            []any{},
		"by_reasoning_effort":        []any{},
		"by_model_family_and_effort": []any{},
		"by_reasoning_token":         []any{},
		"candidate_patterns":         []any{},
		"recent_samples":             []reasoningSample{},
	}
	for key, value := range buildReasoningBehaviorMetadata(runtime) {
		payload[key] = value
	}
	return payload
}

func buildReasoningBehaviorDayFilePath(runtime *appRuntime, dateKey string) string {
	return filepath.Join(runtime.Paths.AnalyticsRoot, fmt.Sprintf("reasoning-behavior-%s.json", dateKey))
}

func mergeSamplesByID(samples []reasoningSample) []reasoningSample {
	merged := map[string]reasoningSample{}
	for _, sample := range samples {
		normalized := clonePlainSample(sample)
		key := firstNonEmpty(normalized.SampleID, normalized.ID)
		if key == "" {
			continue
		}
		merged[key] = normalized
	}
	result := make([]reasoningSample, 0, len(merged))
	for _, sample := range merged {
		result = append(result, sample)
	}
	sort.Slice(result, func(left int, right int) bool {
		return result[left].TS < result[right].TS
	})
	return result
}

func roundMetric(value float64, digits int) any {
	if digits < 0 {
		digits = 0
	}
	scale := 1.0
	for index := 0; index < digits; index++ {
		scale *= 10
	}
	return float64(int(value*scale+0.5)) / scale
}

func averageSampleMetric(samples []reasoningSample, getter func(reasoningSample) (float64, bool), digits int) any {
	total := 0.0
	count := 0
	for _, sample := range samples {
		value, ok := getter(sample)
		if !ok {
			continue
		}
		total += value
		count++
	}
	if count == 0 {
		return nil
	}
	return roundMetric(total/float64(count), digits)
}

func sampleExtraNumber(sample reasoningSample, key string) (float64, bool) {
	switch key {
	case "duration_total_ms":
		if sample.DurationTotalMS != nil {
			return float64(*sample.DurationTotalMS), true
		}
	case "output_tokens":
		if sample.OutputTokens != nil {
			return float64(*sample.OutputTokens), true
		}
	case "total_tokens":
		if sample.TotalTokens != nil {
			return float64(*sample.TotalTokens), true
		}
	case "output_tps":
		if value, ok := numberFromAny(sample.OutputTPS); ok {
			return value, true
		}
	case "reasoning_adjusted_tps":
		if value, ok := numberFromAny(sample.ReasoningAdjustedTPS); ok {
			return value, true
		}
	case "time_normalization_deviation":
		if value, ok := numberFromAny(sample.TimeNormalizationDeviation); ok {
			return value, true
		}
	case "internal_retry_attempt_index":
		return float64(sample.InternalRetryAttemptIndex), true
	case "internal_retry_remaining":
		if sample.InternalRetryRemaining != nil {
			return float64(*sample.InternalRetryRemaining), true
		}
	case "upstream_http_status":
		if sample.UpstreamHTTPStatus != nil {
			return float64(*sample.UpstreamHTTPStatus), true
		}
	}
	if sample.Extra == nil {
		return 0, false
	}
	return numberFromAny(sample.Extra[key])
}

func numberFromAny(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case json.Number:
		value, err := typed.Float64()
		return value, err == nil
	case string:
		value, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return value, err == nil
	default:
		return 0, false
	}
}

func topReasoningTokensForSamples(samples []reasoningSample, limit int) []map[string]any {
	counts := map[int]int{}
	for _, sample := range samples {
		if sample.ReasoningTokens == nil {
			continue
		}
		counts[*sample.ReasoningTokens]++
	}
	type tokenCount struct {
		Value int
		Count int
	}
	entries := make([]tokenCount, 0, len(counts))
	for value, count := range counts {
		entries = append(entries, tokenCount{Value: value, Count: count})
	}
	sort.Slice(entries, func(left int, right int) bool {
		if entries[left].Count != entries[right].Count {
			return entries[left].Count > entries[right].Count
		}
		return entries[left].Value < entries[right].Value
	})
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	total := len(samples)
	if total == 0 {
		total = 1
	}
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		result = append(result, map[string]any{
			"value": entry.Value,
			"count": entry.Count,
			"ratio": roundMetric(float64(entry.Count)/float64(total), 6),
		})
	}
	return result
}

func repeatedReasoningTokensForSamples(samples []reasoningSample, limit int) []map[string]any {
	filtered := make([]map[string]any, 0)
	for _, entry := range topReasoningTokensForSamples(samples, len(samples)) {
		count, _ := entry["count"].(int)
		if count > 1 {
			filtered = append(filtered, map[string]any{
				"value": entry["value"],
				"count": entry["count"],
			})
		}
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered
}

func buildOutputTPSBuckets(samples []reasoningSample) []map[string]any {
	type bucket struct {
		Label string
		Min   float64
		Max   float64
		Count int
	}
	buckets := []bucket{
		{Label: "0-5", Min: 0, Max: 5},
		{Label: "5-15", Min: 5, Max: 15},
		{Label: "15-30", Min: 15, Max: 30},
		{Label: "30+", Min: 30, Max: 1e9},
	}
	for _, sample := range samples {
		value, ok := sampleExtraNumber(sample, "output_tps")
		if !ok {
			continue
		}
		for index := range buckets {
			if value >= buckets[index].Min && value < buckets[index].Max {
				buckets[index].Count++
				break
			}
		}
	}
	result := make([]map[string]any, 0, len(buckets))
	for _, item := range buckets {
		result = append(result, map[string]any{
			"label": item.Label,
			"count": item.Count,
		})
	}
	return result
}

func summarizeCandidatePatternStatus(samples []reasoningSample) string {
	if len(samples) == 0 {
		return "observe_only"
	}
	allContextCompaction := true
	for _, sample := range samples {
		if sample.BlockedByGateway {
			return "blocked"
		}
		if sample.MatchedCurrentRule {
			return "matched_current_rule"
		}
		if sample.InterceptExemptReason != "context_compaction" {
			allContextCompaction = false
		}
	}
	if allContextCompaction {
		return "context_compaction_exempt"
	}
	return "observe_only"
}

func summarizeGroupedSamples(groupEntries map[string][]reasoningSample, totalCount int) []groupedSampleSummary {
	entries := make([]groupedSampleSummary, 0, len(groupEntries))
	for key, samples := range groupEntries {
		count := len(samples)
		finalAnswerOnlyCount := 0
		commentaryCount := 0
		for _, sample := range samples {
			if sample.FinalAnswerOnly {
				finalAnswerOnlyCount++
			}
			if sample.CommentaryObserved {
				commentaryCount++
			}
		}
		ratio := 0.0
		if totalCount > 0 {
			ratio = float64(count) / float64(totalCount)
		}
		finalOnlyRatio := 0.0
		commentaryRatio := 0.0
		if count > 0 {
			finalOnlyRatio = float64(finalAnswerOnlyCount) / float64(count)
			commentaryRatio = float64(commentaryCount) / float64(count)
		}
		entries = append(entries, groupedSampleSummary{
			Key:                     key,
			Count:                   count,
			Ratio:                   ratio,
			FinalAnswerOnlyRatio:    finalOnlyRatio,
			CommentaryPresentRatio:  commentaryRatio,
			CommentaryObservedRatio: commentaryRatio,
			AvgDurationTotalMS:      averageSampleMetric(samples, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "duration_total_ms") }, 2),
			AvgOutputTPS:            averageSampleMetric(samples, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "output_tps") }, 4),
			AvgReasoningAdjustedTPS: averageSampleMetric(samples, func(sample reasoningSample) (float64, bool) {
				return sampleExtraNumber(sample, "reasoning_adjusted_tps")
			}, 4),
			TopReasoningTokens: repeatedReasoningTokensForSamples(samples, 3),
		})
	}
	sort.Slice(entries, func(left int, right int) bool {
		if entries[left].Count != entries[right].Count {
			return entries[left].Count > entries[right].Count
		}
		return entries[left].Key < entries[right].Key
	})
	return entries
}

func buildReasoningBehaviorSnapshotFromSamples(samples []reasoningSample, recentLimit int) map[string]any {
	normalized := make([]reasoningSample, 0, len(samples))
	for _, sample := range samples {
		normalized = append(normalized, clonePlainSample(sample))
	}
	sort.Slice(normalized, func(left int, right int) bool {
		return normalized[left].TS > normalized[right].TS
	})
	totalSamples := len(normalized)
	finalAnswerOnlyCount := 0
	commentaryCount := 0
	byModelFamilyGroups := map[string][]reasoningSample{}
	byReasoningEffortGroups := map[string][]reasoningSample{}
	byFamilyEffortGroups := map[string][]reasoningSample{}
	byReasoningTokenGroups := map[int][]reasoningSample{}
	candidatePatternGroups := map[string][]reasoningSample{}

	for _, sample := range normalized {
		if sample.FinalAnswerOnly {
			finalAnswerOnlyCount++
		}
		if sample.CommentaryObserved {
			commentaryCount++
		}
		modelFamily := firstNonEmpty(sample.EffectiveLocalModelFamily, sample.RequestModelFamily, normalizeModelFamily(sample.RequestModel))
		reasoningEffort := firstNonEmpty(sample.RequestReasoningEffort, "unknown")
		byModelFamilyGroups[modelFamily] = append(byModelFamilyGroups[modelFamily], sample)
		byReasoningEffortGroups[reasoningEffort] = append(byReasoningEffortGroups[reasoningEffort], sample)
		familyEffortKey := modelFamily + "|" + reasoningEffort
		byFamilyEffortGroups[familyEffortKey] = append(byFamilyEffortGroups[familyEffortKey], sample)
		if sample.ReasoningTokens != nil {
			tokenValue := *sample.ReasoningTokens
			byReasoningTokenGroups[tokenValue] = append(byReasoningTokenGroups[tokenValue], sample)
			if sample.FinalAnswerOnly && !sample.CommentaryObserved {
				patternKey := fmt.Sprintf("reasoning=%d|final_answer_only|commentary_not_observed", tokenValue)
				candidatePatternGroups[patternKey] = append(candidatePatternGroups[patternKey], sample)
			}
		}
	}

	byModelFamilyRaw := summarizeGroupedSamples(byModelFamilyGroups, totalSamples)
	byModelFamily := make([]map[string]any, 0, len(byModelFamilyRaw))
	for _, entry := range byModelFamilyRaw {
		byModelFamily = append(byModelFamily, map[string]any{
			"model_family":              entry.Key,
			"count":                     entry.Count,
			"ratio":                     roundMetric(entry.Ratio, 6),
			"final_answer_only_ratio":   roundMetric(entry.FinalAnswerOnlyRatio, 6),
			"commentary_present_ratio":  roundMetric(entry.CommentaryPresentRatio, 6),
			"commentary_observed_ratio": roundMetric(entry.CommentaryObservedRatio, 6),
			"avg_duration_total_ms":     entry.AvgDurationTotalMS,
			"avg_output_tps":            entry.AvgOutputTPS,
			"top_reasoning_tokens":      entry.TopReasoningTokens,
		})
	}

	byReasoningEffortRaw := summarizeGroupedSamples(byReasoningEffortGroups, totalSamples)
	byReasoningEffort := make([]map[string]any, 0, len(byReasoningEffortRaw))
	for _, entry := range byReasoningEffortRaw {
		byReasoningEffort = append(byReasoningEffort, map[string]any{
			"reasoning_effort":           entry.Key,
			"count":                      entry.Count,
			"ratio":                      roundMetric(entry.Ratio, 6),
			"final_answer_only_ratio":    roundMetric(entry.FinalAnswerOnlyRatio, 6),
			"commentary_present_ratio":   roundMetric(entry.CommentaryPresentRatio, 6),
			"commentary_observed_ratio":  roundMetric(entry.CommentaryObservedRatio, 6),
			"avg_duration_total_ms":      entry.AvgDurationTotalMS,
			"avg_reasoning_adjusted_tps": entry.AvgReasoningAdjustedTPS,
			"top_reasoning_tokens":       entry.TopReasoningTokens,
		})
	}

	byModelFamilyAndEffortRaw := summarizeGroupedSamples(byFamilyEffortGroups, totalSamples)
	byModelFamilyAndEffort := make([]map[string]any, 0, len(byModelFamilyAndEffortRaw))
	for _, entry := range byModelFamilyAndEffortRaw {
		parts := strings.SplitN(entry.Key, "|", 2)
		modelFamily := parts[0]
		reasoningEffort := "unknown"
		if len(parts) == 2 {
			reasoningEffort = parts[1]
		}
		byModelFamilyAndEffort = append(byModelFamilyAndEffort, map[string]any{
			"group_key":                 entry.Key,
			"group_label":               modelFamily + " / " + reasoningEffort,
			"model_family":              modelFamily,
			"reasoning_effort":          reasoningEffort,
			"count":                     entry.Count,
			"ratio":                     roundMetric(entry.Ratio, 6),
			"final_answer_only_ratio":   roundMetric(entry.FinalAnswerOnlyRatio, 6),
			"commentary_present_ratio":  roundMetric(entry.CommentaryPresentRatio, 6),
			"commentary_observed_ratio": roundMetric(entry.CommentaryObservedRatio, 6),
			"avg_duration_total_ms":     entry.AvgDurationTotalMS,
			"avg_output_tps":            entry.AvgOutputTPS,
			"top_reasoning_tokens":      entry.TopReasoningTokens,
		})
	}

	type tokenSummary struct {
		Value   int
		Payload map[string]any
	}
	tokenSummaries := make([]tokenSummary, 0, len(byReasoningTokenGroups))
	for value, tokenSamples := range byReasoningTokenGroups {
		finalOnlyCount := 0
		commentaryCount := 0
		lastSeen := ""
		for _, sample := range tokenSamples {
			if sample.FinalAnswerOnly {
				finalOnlyCount++
			}
			if sample.CommentaryObserved {
				commentaryCount++
			}
			if sample.TS > lastSeen {
				lastSeen = sample.TS
			}
		}
		count := len(tokenSamples)
		finalOnlyRatio := 0.0
		commentaryRatio := 0.0
		if count > 0 {
			finalOnlyRatio = float64(finalOnlyCount) / float64(count)
			commentaryRatio = float64(commentaryCount) / float64(count)
		}
		tokenSummaries = append(tokenSummaries, tokenSummary{
			Value: value,
			Payload: map[string]any{
				"value":                     value,
				"count":                     count,
				"final_answer_only_ratio":   roundMetric(finalOnlyRatio, 6),
				"commentary_present_ratio":  roundMetric(commentaryRatio, 6),
				"commentary_observed_ratio": roundMetric(commentaryRatio, 6),
				"avg_duration_total_ms":     averageSampleMetric(tokenSamples, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "duration_total_ms") }, 2),
				"avg_output_tps":            averageSampleMetric(tokenSamples, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "output_tps") }, 4),
				"avg_time_normalization_deviation": averageSampleMetric(tokenSamples, func(sample reasoningSample) (float64, bool) {
					return sampleExtraNumber(sample, "time_normalization_deviation")
				}, 4),
				"last_seen_at": nullIfEmpty(lastSeen),
			},
		})
	}
	sort.Slice(tokenSummaries, func(left int, right int) bool {
		leftCount := tokenSummaries[left].Payload["count"].(int)
		rightCount := tokenSummaries[right].Payload["count"].(int)
		if leftCount != rightCount {
			return leftCount > rightCount
		}
		return tokenSummaries[left].Value < tokenSummaries[right].Value
	})
	byReasoningToken := make([]map[string]any, 0, len(tokenSummaries))
	for _, entry := range tokenSummaries {
		byReasoningToken = append(byReasoningToken, entry.Payload)
	}

	type candidateSummary struct {
		Key     string
		Samples []reasoningSample
	}
	candidateEntries := make([]candidateSummary, 0, len(candidatePatternGroups))
	for key, patternSamples := range candidatePatternGroups {
		candidateEntries = append(candidateEntries, candidateSummary{Key: key, Samples: patternSamples})
	}
	sort.Slice(candidateEntries, func(left int, right int) bool {
		if len(candidateEntries[left].Samples) != len(candidateEntries[right].Samples) {
			return len(candidateEntries[left].Samples) > len(candidateEntries[right].Samples)
		}
		return candidateEntries[left].Key < candidateEntries[right].Key
	})
	candidatePatterns := make([]map[string]any, 0, len(candidateEntries))
	for _, entry := range candidateEntries {
		lastSeen := ""
		for _, sample := range entry.Samples {
			if sample.TS > lastSeen {
				lastSeen = sample.TS
			}
		}
		ratio := 0.0
		if totalSamples > 0 {
			ratio = float64(len(entry.Samples)) / float64(totalSamples)
		}
		candidatePatterns = append(candidatePatterns, map[string]any{
			"pattern_key":           entry.Key,
			"count":                 len(entry.Samples),
			"ratio":                 roundMetric(ratio, 6),
			"avg_duration_total_ms": averageSampleMetric(entry.Samples, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "duration_total_ms") }, 2),
			"avg_output_tps":        averageSampleMetric(entry.Samples, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "output_tps") }, 4),
			"avg_time_normalization_deviation": averageSampleMetric(entry.Samples, func(sample reasoningSample) (float64, bool) {
				return sampleExtraNumber(sample, "time_normalization_deviation")
			}, 4),
			"last_seen_at": nullIfEmpty(lastSeen),
			"status":       summarizeCandidatePatternStatus(entry.Samples),
		})
	}

	recent := append([]reasoningSample{}, normalized...)
	if recentLimit > 0 && len(recent) > recentLimit {
		recent = recent[:recentLimit]
	}
	finalAnswerOnlyRatio := 0.0
	commentaryRatio := 0.0
	if totalSamples > 0 {
		finalAnswerOnlyRatio = float64(finalAnswerOnlyCount) / float64(totalSamples)
		commentaryRatio = float64(commentaryCount) / float64(totalSamples)
	}
	return map[string]any{
		"schema_version":  reasoningBehaviorSchemaVersion,
		"analytics_ready": true,
		"summary": map[string]any{
			"total_samples":             totalSamples,
			"final_answer_only_ratio":   roundMetric(finalAnswerOnlyRatio, 6),
			"commentary_present_ratio":  roundMetric(commentaryRatio, 6),
			"commentary_observed_ratio": roundMetric(commentaryRatio, 6),
			"avg_duration_total_ms":     averageSampleMetric(normalized, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "duration_total_ms") }, 2),
			"avg_output_tps":            averageSampleMetric(normalized, func(sample reasoningSample) (float64, bool) { return sampleExtraNumber(sample, "output_tps") }, 4),
			"avg_reasoning_adjusted_tps": averageSampleMetric(normalized, func(sample reasoningSample) (float64, bool) {
				return sampleExtraNumber(sample, "reasoning_adjusted_tps")
			}, 4),
			"wording": "summary reflects observed response structure only",
		},
		"top_reasoning_tokens":       topReasoningTokensForSamples(normalized, 8),
		"output_tps_buckets":         buildOutputTPSBuckets(normalized),
		"by_model_family":            byModelFamily,
		"by_reasoning_effort":        byReasoningEffort,
		"by_model_family_and_effort": byModelFamilyAndEffort,
		"by_reasoning_token":         byReasoningToken,
		"candidate_patterns":         candidatePatterns,
		"recent_samples":             recent,
	}
}

func nullIfEmpty(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func readReasoningBehaviorDayFile(runtime *appRuntime, dateKey string) ([]reasoningSample, error) {
	body, err := os.ReadFile(buildReasoningBehaviorDayFilePath(runtime, dateKey))
	if err != nil {
		if os.IsNotExist(err) {
			return []reasoningSample{}, nil
		}
		return nil, err
	}
	var payload reasoningBehaviorDayFile
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if payload.Samples == nil {
		return []reasoningSample{}, nil
	}
	result := make([]reasoningSample, 0, len(payload.Samples))
	for _, sample := range payload.Samples {
		result = append(result, clonePlainSample(sample))
	}
	return result, nil
}

func flushReasoningBehaviorDay(runtime *appRuntime, dateKey string) error {
	runtime.ReasoningMu.Lock()
	bufferedSamples := append([]reasoningSample{}, runtime.ReasoningBehavior.DailyBuffers[dateKey]...)
	runtime.ReasoningMu.Unlock()

	existingSamples, err := readReasoningBehaviorDayFile(runtime, dateKey)
	if err != nil {
		return err
	}
	mergedSamples := mergeSamplesByID(append(existingSamples, bufferedSamples...))
	if err := os.MkdirAll(runtime.Paths.AnalyticsRoot, 0o755); err != nil {
		return err
	}
	snapshot := buildReasoningBehaviorSnapshotFromSamples(mergedSamples, 50)
	dailySummary, _ := snapshot["summary"].(map[string]any)
	payload := reasoningBehaviorDayFile{
		Date:          dateKey,
		SchemaVersion: reasoningBehaviorSchemaVersion,
		GeneratedBy:   "codex-retry-gateway",
		Samples:       mergedSamples,
		DailySummary:  dailySummary,
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(buildReasoningBehaviorDayFilePath(runtime, dateKey), append(body, '\n'), 0o644); err != nil {
		return err
	}

	runtime.ReasoningMu.Lock()
	runtime.ReasoningBehavior.LastFlushAt = time.Now().Format(time.RFC3339)
	runtime.ReasoningBehavior.LastFlushError = ""
	runtime.ReasoningMu.Unlock()
	return nil
}

func (runtime *appRuntime) scheduleReasoningBehaviorFlushLocked(dateKey string) {
	if existingTimer := runtime.ReasoningBehavior.FlushTimers[dateKey]; existingTimer != nil {
		existingTimer.Stop()
	}
	timer := time.AfterFunc(30*time.Millisecond, func() {
		runtime.ReasoningMu.Lock()
		delete(runtime.ReasoningBehavior.FlushTimers, dateKey)
		runtime.ReasoningMu.Unlock()
		if err := flushReasoningBehaviorDay(runtime, dateKey); err != nil {
			runtime.ReasoningMu.Lock()
			runtime.ReasoningBehavior.LastFlushError = err.Error()
			runtime.ReasoningMu.Unlock()
			runtime.Logger(fmt.Sprintf("[analytics-error] reasoning flush failed date=%s message=%s", dateKey, err.Error()))
		}
	})
	runtime.ReasoningBehavior.FlushTimers[dateKey] = timer
}

func readReasoningBehaviorSamplesByDateKey(runtime *appRuntime, dateKey string) ([]reasoningSample, error) {
	combined := make([]reasoningSample, 0)
	fileSamples, err := readReasoningBehaviorDayFile(runtime, dateKey)
	if err != nil {
		return nil, err
	}
	combined = append(combined, fileSamples...)
	runtime.ReasoningMu.Lock()
	bufferedSamples := append([]reasoningSample{}, runtime.ReasoningBehavior.DailyBuffers[dateKey]...)
	runtime.ReasoningMu.Unlock()
	combined = append(combined, bufferedSamples...)
	return mergeSamplesByID(combined), nil
}

func readReasoningBehaviorSamplesByDateRange(runtime *appRuntime, dateFrom string, dateTo string) ([]reasoningSample, error) {
	normalizedFrom := normalizeDateKeyInput(dateFrom)
	normalizedTo := normalizeDateKeyInput(dateTo)
	combined := make([]reasoningSample, 0)

	entries, err := os.ReadDir(runtime.Paths.AnalyticsRoot)
	if err == nil {
		for _, entry := range entries {
			match := reasoningDayFileNameRegexp.FindStringSubmatch(entry.Name())
			if len(match) != 2 {
				continue
			}
			dateKey := match[1]
			if !isDateKeyWithinRange(dateKey, normalizedFrom, normalizedTo) {
				continue
			}
			samples, readErr := readReasoningBehaviorDayFile(runtime, dateKey)
			if readErr != nil {
				return nil, readErr
			}
			combined = append(combined, samples...)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	runtime.ReasoningMu.Lock()
	for dateKey, bufferedSamples := range runtime.ReasoningBehavior.DailyBuffers {
		if !isDateKeyWithinRange(dateKey, normalizedFrom, normalizedTo) {
			continue
		}
		combined = append(combined, append([]reasoningSample{}, bufferedSamples...)...)
	}
	runtime.ReasoningMu.Unlock()

	merged := mergeSamplesByID(combined)
	sort.Slice(merged, func(left int, right int) bool {
		return merged[left].TS > merged[right].TS
	})
	return merged, nil
}

func csvQuote(value any) string {
	text := ""
	if value != nil {
		text = fmt.Sprintf("%v", value)
	}
	return `"` + strings.ReplaceAll(text, `"`, `""`) + `"`
}

func buildReasoningBehaviorCSV(samples []reasoningSample) string {
	headers := []string{
		"sample_id",
		"gateway_request_id",
		"attempt_id",
		"ts",
		"path",
		"method",
		"request_kind",
		"intercept_exempt_reason",
		"request_model",
		"effective_local_model_family",
		"request_reasoning_effort",
		"reasoning_tokens",
		"output_tokens",
		"total_tokens",
		"duration_total_ms",
		"output_tps",
		"reasoning_adjusted_tps",
		"final_answer_only",
		"has_commentary",
		"commentary_observed",
		"has_final_answer",
		"has_tool_call",
		"has_reasoning_item",
		"time_to_first_chunk_ms",
		"time_to_first_content_ms",
		"stream_duration_ms",
		"matched_current_rule",
		"blocked_by_gateway",
		"upstream_stream_terminated",
		"internal_retry_attempt_index",
		"internal_retry_remaining",
		"final_action",
		"upstream_http_status",
		"client_http_status",
		"body_bytes",
		"body_sha256",
		"request_payload_excerpt",
		"failure_code",
		"failure_message",
	}
	lines := []string{strings.Join(headers, ",")}
	for _, rawSample := range samples {
		sample := clonePlainSample(rawSample)
		var failureCode any
		var failureMessage any
		if sample.FailureSummary != nil {
			failureCode = sample.FailureSummary["code"]
			failureMessage = sample.FailureSummary["message"]
		}
		var bodyBytes any
		var bodySHA any
		if sample.RequestSummary != nil {
			bodyBytes = sample.RequestSummary["body_bytes"]
			bodySHA = sample.RequestSummary["body_sha256"]
		}
		values := []string{
			csvQuote(sample.SampleID),
			csvQuote(sample.GatewayRequestID),
			csvQuote(sample.AttemptID),
			csvQuote(sample.TS),
			csvQuote(sample.Path),
			csvQuote(sample.Method),
			csvQuote(sample.RequestKind),
			csvQuote(sample.InterceptExemptReason),
			csvQuote(sample.RequestModel),
			csvQuote(sample.EffectiveLocalModelFamily),
			csvQuote(sample.RequestReasoningEffort),
			csvQuote(reasoningValue(sample.ReasoningTokens)),
			csvQuote(reasoningValue(sample.OutputTokens)),
			csvQuote(reasoningValue(sample.TotalTokens)),
			csvQuote(reasoningValue(sample.DurationTotalMS)),
			csvQuote(sample.OutputTPS),
			csvQuote(sample.ReasoningAdjustedTPS),
			csvQuote(sample.FinalAnswerOnly),
			csvQuote(sample.HasCommentary),
			csvQuote(sample.CommentaryObserved),
			csvQuote(sample.HasFinalAnswer),
			csvQuote(sample.HasToolCall),
			csvQuote(sample.HasReasoningItem),
			csvQuote(nil),
			csvQuote(nil),
			csvQuote(nil),
			csvQuote(sample.MatchedCurrentRule),
			csvQuote(sample.BlockedByGateway),
			csvQuote(sample.UpstreamStreamTerminated),
			csvQuote(sample.InternalRetryAttemptIndex),
			csvQuote(reasoningValue(sample.InternalRetryRemaining)),
			csvQuote(sample.FinalAction),
			csvQuote(reasoningValue(sample.UpstreamHTTPStatus)),
			csvQuote(sample.ClientHTTPStatus),
			csvQuote(bodyBytes),
			csvQuote(bodySHA),
			csvQuote(sample.RequestPayloadExcerpt),
			csvQuote(failureCode),
			csvQuote(failureMessage),
		}
		lines = append(lines, strings.Join(values, ","))
	}
	return strings.Join(lines, "\n")
}

func buildReasoningExportJobPublic(job *reasoningExportJob) map[string]any {
	if job == nil {
		return nil
	}
	percent := 0.0
	if job.TotalDays > 0 {
		percent = float64(job.ProcessedDays) / float64(job.TotalDays)
		if percent > 1 {
			percent = 1
		}
	}
	downloadURL := any(nil)
	if job.Status == "completed" {
		downloadURL = fmt.Sprintf("%s/jobs/%s/download", reasoningBehaviorExportPath, job.JobID)
	}
	return map[string]any{
		"job_id":        job.JobID,
		"status":        job.Status,
		"format":        job.Format,
		"date_from":     nullIfEmpty(job.DateFrom),
		"date_to":       nullIfEmpty(job.DateTo),
		"created_at":    nullIfEmpty(job.CreatedAt),
		"started_at":    nullIfEmpty(job.StartedAt),
		"finished_at":   nullIfEmpty(job.FinishedAt),
		"error_message": nullIfEmpty(job.ErrorMessage),
		"output_path":   nullIfEmpty(job.OutputPath),
		"download_url":  downloadURL,
		"progress": map[string]any{
			"total_days":     job.TotalDays,
			"processed_days": job.ProcessedDays,
			"sample_count":   job.SampleCount,
			"percent":        roundMetric(percent, 4),
		},
	}
}

func trimReasoningExportJobs(state *reasoningBehaviorState) {
	jobs := make([]*reasoningExportJob, 0, len(state.ExportJobs))
	for _, job := range state.ExportJobs {
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(left int, right int) bool {
		return jobs[left].CreatedAt > jobs[right].CreatedAt
	})
	if len(jobs) <= reasoningBehaviorExportJobLimit {
		return
	}
	for _, job := range jobs[reasoningBehaviorExportJobLimit:] {
		if job.Status == "queued" || job.Status == "running" {
			continue
		}
		delete(state.ExportJobs, job.JobID)
	}
}

func writeReasoningExportJobFile(runtime *appRuntime, job *reasoningExportJob, samples []reasoningSample) (string, error) {
	exportRoot := filepath.Join(runtime.Paths.AnalyticsRoot, "exports", job.JobID)
	if err := os.MkdirAll(exportRoot, 0o755); err != nil {
		return "", err
	}
	extension := "json"
	if job.Format == "csv" {
		extension = "csv"
	}
	outputPath := filepath.Join(exportRoot, "reasoning-export."+extension)
	if job.Format == "csv" {
		if err := os.WriteFile(outputPath, []byte(buildReasoningBehaviorCSV(samples)), 0o644); err != nil {
			return "", err
		}
		return outputPath, nil
	}
	snapshot := buildReasoningBehaviorSnapshotFromSamples(samples, minInt(len(samples), 200))
	payload := map[string]any{
		"ok":                true,
		"exported_at":       time.Now().Format(time.RFC3339),
		"date_from":         nullIfEmpty(job.DateFrom),
		"date_to":           nullIfEmpty(job.DateTo),
		"schema_version":    reasoningBehaviorSchemaVersion,
		"background_export": true,
		"export_job_id":     job.JobID,
		"samples":           samples,
	}
	for key, value := range buildReasoningBehaviorMetadata(runtime) {
		payload[key] = value
	}
	for key, value := range snapshot {
		payload[key] = value
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

func runReasoningExportJob(runtime *appRuntime, job *reasoningExportJob) {
	runtime.ReasoningMu.Lock()
	job.Status = "running"
	job.StartedAt = time.Now().Format(time.RFC3339)
	runtime.ReasoningMu.Unlock()

	samples := make([]reasoningSample, 0)
	for _, dateKey := range job.DateKeys {
		daySamples, err := readReasoningBehaviorSamplesByDateKey(runtime, dateKey)
		if err != nil {
			runtime.ReasoningMu.Lock()
			job.Status = "failed"
			job.ErrorMessage = err.Error()
			job.FinishedAt = time.Now().Format(time.RFC3339)
			runtime.ReasoningMu.Unlock()
			runtime.Logger(fmt.Sprintf("[analytics-error] reasoning background export failed job=%s message=%s", job.JobID, err.Error()))
			return
		}
		samples = append(samples, daySamples...)
		runtime.ReasoningMu.Lock()
		job.ProcessedDays++
		job.SampleCount = len(samples)
		runtime.ReasoningMu.Unlock()
	}

	mergedSamples := mergeSamplesByID(samples)
	sort.Slice(mergedSamples, func(left int, right int) bool {
		return mergedSamples[left].TS > mergedSamples[right].TS
	})
	outputPath, err := writeReasoningExportJobFile(runtime, job, mergedSamples)
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	if err != nil {
		job.Status = "failed"
		job.ErrorMessage = err.Error()
		job.FinishedAt = time.Now().Format(time.RFC3339)
		runtime.Logger(fmt.Sprintf("[analytics-error] reasoning background export failed job=%s message=%s", job.JobID, err.Error()))
		return
	}
	job.SampleCount = len(mergedSamples)
	job.OutputPath = outputPath
	job.Status = "completed"
	job.FinishedAt = time.Now().Format(time.RFC3339)
}

func startReasoningExportJob(runtime *appRuntime, format string, dateFrom string, dateTo string) *reasoningExportJob {
	normalizedFormat := "json"
	if strings.EqualFold(strings.TrimSpace(format), "csv") {
		normalizedFormat = "csv"
	}
	dateKeys := listInclusiveDateKeys(dateFrom, dateTo)
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	sequence := runtime.ReasoningBehavior.NextExportJobSequence
	runtime.ReasoningBehavior.NextExportJobSequence++
	job := &reasoningExportJob{
		JobID:         fmt.Sprintf("reasoning_export_%d_%d", time.Now().UnixMilli(), sequence),
		Status:        "queued",
		Format:        normalizedFormat,
		DateFrom:      dateFrom,
		DateTo:        dateTo,
		DateKeys:      dateKeys,
		TotalDays:     len(dateKeys),
		ProcessedDays: 0,
		SampleCount:   0,
		CreatedAt:     time.Now().Format(time.RFC3339),
	}
	runtime.ReasoningBehavior.ExportJobs[job.JobID] = job
	trimReasoningExportJobs(runtime.ReasoningBehavior)
	go runReasoningExportJob(runtime, job)
	return job
}

func getReasoningExportJob(runtime *appRuntime, jobID string) *reasoningExportJob {
	runtime.ReasoningMu.Lock()
	defer runtime.ReasoningMu.Unlock()
	job := runtime.ReasoningBehavior.ExportJobs[jobID]
	if job == nil {
		return nil
	}
	cloned := *job
	cloned.DateKeys = append([]string{}, job.DateKeys...)
	return &cloned
}

func normalizeAnalysisStringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		result := make([]string, 0, len(typed))
		for _, entry := range typed {
			text := strings.TrimSpace(strings.ToLower(anyToString(entry)))
			if text != "" {
				result = append(result, text)
			}
		}
		return result
	case []string:
		result := make([]string, 0, len(typed))
		for _, entry := range typed {
			text := strings.TrimSpace(strings.ToLower(entry))
			if text != "" {
				result = append(result, text)
			}
		}
		return result
	case string:
		parts := strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t'
		})
		result := make([]string, 0, len(parts))
		for _, entry := range parts {
			text := strings.TrimSpace(strings.ToLower(entry))
			if text != "" {
				result = append(result, text)
			}
		}
		return result
	default:
		return []string{}
	}
}

func normalizeNumberList(value any) []int {
	rawValues := []string{}
	switch typed := value.(type) {
	case []any:
		for _, entry := range typed {
			rawValues = append(rawValues, anyToString(entry))
		}
	case []string:
		rawValues = append(rawValues, typed...)
	case string:
		rawValues = strings.FieldsFunc(typed, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t'
		})
	case nil:
		return []int{}
	default:
		rawValues = append(rawValues, anyToString(typed))
	}
	result := make([]int, 0, len(rawValues))
	for _, entry := range rawValues {
		value, err := strconv.Atoi(strings.TrimSpace(entry))
		if err == nil {
			result = append(result, value)
		}
	}
	return result
}

func sampleModelFamily(sample reasoningSample) string {
	return firstNonEmpty(sample.EffectiveLocalModelFamily, sample.RequestModelFamily, normalizeModelFamily(sample.RequestModel))
}

func sampleReasoningEffort(sample reasoningSample) string {
	value := strings.TrimSpace(strings.ToLower(sample.RequestReasoningEffort))
	switch value {
	case "low", "medium", "high", "xhigh":
		return value
	default:
		return ""
	}
}

func sampleHasAnalysisField(sample reasoningSample, field string) bool {
	switch field {
	case "reasoning_tokens":
		return sample.ReasoningTokens != nil
	case "final_answer_only":
		return true
	case "commentary_observed":
		return true
	case "duration_total_ms":
		_, ok := sampleExtraNumber(sample, "duration_total_ms")
		return ok
	case "output_tokens":
		_, ok := sampleExtraNumber(sample, "output_tokens")
		return ok
	case "model_family":
		return sampleModelFamily(sample) != ""
	case "reasoning_effort":
		return sampleReasoningEffort(sample) != ""
	case "status":
		return sample.FinalAction != "" || sample.ClientHTTPStatus != nil
	case "retry_status":
		_, hasIndex := sampleExtraNumber(sample, "internal_retry_attempt_index")
		_, hasRemaining := sampleExtraNumber(sample, "internal_retry_remaining")
		return hasIndex || hasRemaining
	case "blocked_status":
		return true
	default:
		return false
	}
}

func calculateFieldCoverage(samples []reasoningSample) map[string]any {
	total := len(samples)
	coverage := map[string]any{}
	for _, field := range reasoningAnalysisFields {
		count := 0
		for _, sample := range samples {
			if sampleHasAnalysisField(sample, field) {
				count++
			}
		}
		if total == 0 {
			coverage[field] = 0
		} else {
			coverage[field] = roundMetric(float64(count)/float64(total), 6)
		}
	}
	return coverage
}

func asFloat(value any) float64 {
	switch typed := value.(type) {
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float64:
		return typed
	case float32:
		return float64(typed)
	default:
		return 0
	}
}

func decideReasoningAnalysisValue(fieldCoverage map[string]any, sampleCount int) map[string]any {
	missingCoreFields := make([]string, 0)
	for _, field := range reasoningAnalysisCoreFields {
		if asFloat(fieldCoverage[field]) <= 0 {
			missingCoreFields = append(missingCoreFields, field)
		}
	}
	if sampleCount <= 0 {
		return map[string]any{
			"analysis_value":               "no_analysis_value",
			"can_build_reasoning_features": false,
			"can_build_candidate_patterns": false,
			"missing_core_fields":          reasoningAnalysisCoreFields,
			"decision_reason":              "no structured reasoning samples are available",
		}
	}
	if len(missingCoreFields) > 0 {
		return map[string]any{
			"analysis_value":               "no_analysis_value",
			"can_build_reasoning_features": false,
			"can_build_candidate_patterns": false,
			"missing_core_fields":          missingCoreFields,
			"decision_reason":              "missing core reasoning analysis fields",
		}
	}
	missingSupportFields := make([]string, 0)
	for _, field := range []string{"duration_total_ms", "output_tokens", "model_family", "reasoning_effort"} {
		if asFloat(fieldCoverage[field]) <= 0 {
			missingSupportFields = append(missingSupportFields, field)
		}
	}
	if len(missingSupportFields) > 0 {
		return map[string]any{
			"analysis_value":               "partial",
			"can_build_reasoning_features": true,
			"can_build_candidate_patterns": false,
			"missing_core_fields":          []string{},
			"decision_reason":              "supporting reasoning analysis fields are incomplete",
		}
	}
	return map[string]any{
		"analysis_value":               "valuable",
		"can_build_reasoning_features": true,
		"can_build_candidate_patterns": true,
		"missing_core_fields":          []string{},
		"decision_reason":              "reasoning samples contain enough structure for feature analysis",
	}
}

func buildReasoningAnalysisProfile(payload map[string]any, dataSource string) reasoningAnalysisProfile {
	filters, _ := payload["filters"].(map[string]any)
	conditions, _ := payload["conditions"].(map[string]any)
	reasoningTokens := normalizeNumberList(firstNonNil(conditions["reasoning_tokens"], filters["reasoning_tokens"], []int{516}))
	if len(reasoningTokens) == 0 {
		reasoningTokens = []int{516}
	}
	return reasoningAnalysisProfile{
		Name:       reasoningAnalysisProfileName,
		DataSource: firstNonEmpty(dataSource, "runtime"),
		Filters: reasoningAnalysisFilters{
			DateFrom:        normalizeDateKeyInput(anyToString(firstNonNil(filters["date_from"], payload["date_from"]))),
			DateTo:          normalizeDateKeyInput(anyToString(firstNonNil(filters["date_to"], payload["date_to"]))),
			ModelFamily:     normalizeAnalysisStringList(filters["model_family"]),
			Model:           normalizeAnalysisStringList(filters["model"]),
			ReasoningEffort: normalizeAnalysisStringList(filters["reasoning_effort"]),
			Status:          firstNonEmpty(strings.TrimSpace(strings.ToLower(anyToString(filters["status"]))), "any"),
			IncludeRetries:  filters["include_retries"] != false,
			IncludeBlocked:  filters["include_blocked"] != false,
		},
		Conditions: reasoningAnalysisConditions{
			ReasoningTokens:         reasoningTokens,
			ReasoningTokensMode:     firstNonEmpty(strings.TrimSpace(strings.ToLower(anyToString(conditions["reasoning_tokens_mode"]))), "equals_or_outlier"),
			FinalAnswerOnly:         conditions["final_answer_only"] != false,
			CommentaryNotObserved:   conditions["commentary_not_observed"] != false,
			TimeNormalizationFactor: firstNonEmpty(strings.TrimSpace(strings.ToLower(anyToString(conditions["time_normalization_deviation"]))), "high"),
		},
		Baseline: map[string]any{
			"group_by":                           []string{"model_family", "reasoning_effort", "token_scale_bucket"},
			"compare_with_non_candidate_samples": true,
		},
	}
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func sampleMatchesReasoningAnalysisFilters(sample reasoningSample, profile reasoningAnalysisProfile) bool {
	if len(profile.Filters.ModelFamily) > 0 {
		match := false
		modelFamily := strings.ToLower(sampleModelFamily(sample))
		for _, candidate := range profile.Filters.ModelFamily {
			if candidate == modelFamily {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if len(profile.Filters.Model) > 0 {
		match := false
		requestModel := strings.ToLower(sample.RequestModel)
		for _, candidate := range profile.Filters.Model {
			if candidate == requestModel {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if len(profile.Filters.ReasoningEffort) > 0 {
		match := false
		effort := sampleReasoningEffort(sample)
		for _, candidate := range profile.Filters.ReasoningEffort {
			if candidate == effort {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if !profile.Filters.IncludeRetries {
		if value, ok := sampleExtraNumber(sample, "internal_retry_attempt_index"); ok && value > 0 {
			return false
		}
	}
	if !profile.Filters.IncludeBlocked && sample.BlockedByGateway {
		return false
	}
	switch profile.Filters.Status {
	case "blocked":
		return sample.BlockedByGateway
	case "success":
		return sample.ClientHTTPStatus == nil || *sample.ClientHTTPStatus < 400
	case "upstream_failed":
		return sample.FinalAction == "upstream_fetch_failed"
	case "gateway_rejected":
		return sample.FinalAction == "request_rejected"
	default:
		return true
	}
}

func sampleHasHighTimeNormalizationDeviation(sample reasoningSample, profile reasoningAnalysisProfile) bool {
	if profile.Conditions.TimeNormalizationFactor != "high" {
		return true
	}
	value, ok := sampleExtraNumber(sample, "time_normalization_deviation")
	return ok && value >= 0.5
}

func sampleMatchesCandidateProfile(sample reasoningSample, profile reasoningAnalysisProfile) bool {
	if len(profile.Conditions.ReasoningTokens) > 0 {
		if sample.ReasoningTokens == nil {
			return false
		}
		match := false
		for _, token := range profile.Conditions.ReasoningTokens {
			if token == *sample.ReasoningTokens {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if profile.Conditions.FinalAnswerOnly && !sample.FinalAnswerOnly {
		return false
	}
	if profile.Conditions.CommentaryNotObserved && sample.CommentaryObserved {
		return false
	}
	return sampleHasHighTimeNormalizationDeviation(sample, profile)
}

func averageForSamples(samples []reasoningSample, getter func(reasoningSample) (float64, bool)) any {
	return averageSampleMetric(samples, getter, 6)
}

func buildAnalysisSamplesPreview(samples []reasoningSample) []map[string]any {
	limit := 20
	if len(samples) < limit {
		limit = len(samples)
	}
	preview := make([]map[string]any, 0, limit)
	for _, rawSample := range samples[:limit] {
		sample := clonePlainSample(rawSample)
		var clientStatus any
		if sample.ClientHTTPStatus != nil {
			clientStatus = *sample.ClientHTTPStatus
		}
		outputTokens := reasoningValue(sample.OutputTokens)
		totalTokens := reasoningValue(sample.TotalTokens)
		durationTotalMS := reasoningValue(sample.DurationTotalMS)
		outputTPS := sample.OutputTPS
		timeNormalizationDeviation := sample.TimeNormalizationDeviation
		preview = append(preview, map[string]any{
			"sample_id":                    nullIfEmpty(sample.SampleID),
			"ts":                           nullIfEmpty(sample.TS),
			"request_model":                nullIfEmpty(sample.RequestModel),
			"model_family":                 nullIfEmpty(sampleModelFamily(sample)),
			"reasoning_effort":             nullIfEmpty(sampleReasoningEffort(sample)),
			"reasoning_tokens":             reasoningValue(sample.ReasoningTokens),
			"output_tokens":                outputTokens,
			"total_tokens":                 totalTokens,
			"duration_total_ms":            durationTotalMS,
			"output_tps":                   outputTPS,
			"time_normalization_deviation": timeNormalizationDeviation,
			"final_answer_only":            sample.FinalAnswerOnly,
			"commentary_observed":          sample.CommentaryObserved,
			"final_action":                 nullIfEmpty(sample.FinalAction),
			"client_http_status":           clientStatus,
			"matched_current_rule":         sample.MatchedCurrentRule,
			"blocked_by_gateway":           sample.BlockedByGateway,
			"internal_retry_attempt_index": sample.InternalRetryAttemptIndex,
		})
	}
	return preview
}

func buildFeatureAnalysisFromSamples(samples []reasoningSample, profile reasoningAnalysisProfile) map[string]any {
	allSamples := make([]reasoningSample, 0, len(samples))
	for _, sample := range samples {
		allSamples = append(allSamples, clonePlainSample(sample))
	}
	filteredSamples := make([]reasoningSample, 0, len(allSamples))
	for _, sample := range allSamples {
		if sampleMatchesReasoningAnalysisFilters(sample, profile) {
			filteredSamples = append(filteredSamples, sample)
		}
	}
	fieldCoverage := calculateFieldCoverage(filteredSamples)
	valueDecision := decideReasoningAnalysisValue(fieldCoverage, len(filteredSamples))
	commentaryNotObservedCount := 0
	highDeviationCount := 0
	reasoning516Count := 0
	for _, sample := range filteredSamples {
		if sample.ReasoningTokens != nil && *sample.ReasoningTokens == 516 {
			reasoning516Count++
		}
		if !sample.CommentaryObserved {
			commentaryNotObservedCount++
		}
		if sampleHasHighTimeNormalizationDeviation(sample, profile) {
			highDeviationCount++
		}
	}
	baseResult := map[string]any{
		"ok":                      true,
		"analysis_profile":        profile.Name,
		"analysis_profile_detail": profile,
		"analysis_value":          valueDecision["analysis_value"],
		"conclusion":              "not_observed",
		"field_coverage":          fieldCoverage,
		"missing_core_fields":     valueDecision["missing_core_fields"],
		"decision_reason":         valueDecision["decision_reason"],
		"sample_count":            len(filteredSamples),
		"candidate_summary": map[string]any{
			"candidate_count":                         0,
			"candidate_ratio":                         0,
			"reasoning_516_count":                     reasoning516Count,
			"commentary_not_observed_count":           commentaryNotObservedCount,
			"high_time_normalization_deviation_count": highDeviationCount,
			"last_seen_at":                            nil,
		},
		"baseline_comparison": map[string]any{
			"baseline_count": 0,
			"candidate_avg_time_normalization_deviation": 0,
			"baseline_avg_time_normalization_deviation":  0,
			"candidate_final_answer_only_ratio":          0,
			"baseline_final_answer_only_ratio":           0,
			"candidate_commentary_not_observed_ratio":    0,
			"baseline_commentary_not_observed_ratio":     0,
		},
		"samples_preview": buildAnalysisSamplesPreview(filteredSamples),
	}
	switch valueDecision["analysis_value"] {
	case "no_analysis_value":
		baseResult["conclusion"] = "no_analysis_value"
		return baseResult
	case "partial":
		baseResult["conclusion"] = "insufficient_fields"
		return baseResult
	}

	candidateSamples := make([]reasoningSample, 0)
	candidateIDs := map[string]bool{}
	for _, sample := range filteredSamples {
		if sampleMatchesCandidateProfile(sample, profile) {
			candidateSamples = append(candidateSamples, sample)
			candidateIDs[firstNonEmpty(sample.SampleID, sample.ID)] = true
		}
	}
	baselineSamples := make([]reasoningSample, 0)
	for _, sample := range filteredSamples {
		if !candidateIDs[firstNonEmpty(sample.SampleID, sample.ID)] {
			baselineSamples = append(baselineSamples, sample)
		}
	}
	candidateCount := len(candidateSamples)
	baselineCount := len(baselineSamples)
	candidateRatio := 0.0
	if len(filteredSamples) > 0 {
		candidateRatio = float64(candidateCount) / float64(len(filteredSamples))
	}
	baselineFinalOnlyRatio := 0.0
	baselineCommentaryNotObservedRatio := 0.0
	if baselineCount > 0 {
		finalOnlyCount := 0
		commentaryMissingCount := 0
		for _, sample := range baselineSamples {
			if sample.FinalAnswerOnly {
				finalOnlyCount++
			}
			if !sample.CommentaryObserved {
				commentaryMissingCount++
			}
		}
		baselineFinalOnlyRatio = float64(finalOnlyCount) / float64(baselineCount)
		baselineCommentaryNotObservedRatio = float64(commentaryMissingCount) / float64(baselineCount)
	}
	conclusion := "not_observed"
	if candidateCount > 0 {
		if baselineCount > 0 && baselineFinalOnlyRatio >= 0.5 && baselineCommentaryNotObservedRatio >= 0.5 {
			conclusion = "high_false_positive_risk"
		} else if candidateCount >= 3 {
			conclusion = "strong_candidate"
		} else {
			conclusion = "candidate"
		}
	}
	commentaryMissingCandidateCount := 0
	finalOnlyCandidateCount := 0
	lastSeenAt := ""
	for _, sample := range candidateSamples {
		if sample.FinalAnswerOnly {
			finalOnlyCandidateCount++
		}
		if !sample.CommentaryObserved {
			commentaryMissingCandidateCount++
		}
		if sample.TS > lastSeenAt {
			lastSeenAt = sample.TS
		}
	}
	baseResult["conclusion"] = conclusion
	baseResult["candidate_summary"] = map[string]any{
		"candidate_count":                         candidateCount,
		"candidate_ratio":                         roundMetric(candidateRatio, 6),
		"reasoning_516_count":                     countReasoningToken(candidateSamples, 516),
		"final_answer_only_count":                 finalOnlyCandidateCount,
		"commentary_not_observed_count":           commentaryMissingCandidateCount,
		"high_time_normalization_deviation_count": countHighDeviation(candidateSamples, profile),
		"last_seen_at":                            nullIfEmpty(lastSeenAt),
	}
	baseResult["baseline_comparison"] = map[string]any{
		"baseline_count": baselineCount,
		"candidate_avg_time_normalization_deviation": averageForSamples(candidateSamples, func(sample reasoningSample) (float64, bool) {
			return sampleExtraNumber(sample, "time_normalization_deviation")
		}),
		"baseline_avg_time_normalization_deviation": averageForSamples(baselineSamples, func(sample reasoningSample) (float64, bool) {
			return sampleExtraNumber(sample, "time_normalization_deviation")
		}),
		"candidate_final_answer_only_ratio":       ratioForBool(candidateSamples, func(sample reasoningSample) bool { return sample.FinalAnswerOnly }),
		"baseline_final_answer_only_ratio":        roundMetric(baselineFinalOnlyRatio, 6),
		"candidate_commentary_not_observed_ratio": ratioForBool(candidateSamples, func(sample reasoningSample) bool { return !sample.CommentaryObserved }),
		"baseline_commentary_not_observed_ratio":  roundMetric(baselineCommentaryNotObservedRatio, 6),
	}
	if candidateCount > 0 {
		baseResult["samples_preview"] = buildAnalysisSamplesPreview(candidateSamples)
	}
	return baseResult
}

func countReasoningToken(samples []reasoningSample, value int) int {
	count := 0
	for _, sample := range samples {
		if sample.ReasoningTokens != nil && *sample.ReasoningTokens == value {
			count++
		}
	}
	return count
}

func countHighDeviation(samples []reasoningSample, profile reasoningAnalysisProfile) int {
	count := 0
	for _, sample := range samples {
		if sampleHasHighTimeNormalizationDeviation(sample, profile) {
			count++
		}
	}
	return count
}

func ratioForBool(samples []reasoningSample, predicate func(reasoningSample) bool) any {
	if len(samples) == 0 {
		return 0
	}
	count := 0
	for _, sample := range samples {
		if predicate(sample) {
			count++
		}
	}
	return roundMetric(float64(count)/float64(len(samples)), 6)
}

func minInt(left int, right int) int {
	if left < right {
		return left
	}
	return right
}
