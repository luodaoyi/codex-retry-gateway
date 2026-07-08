package main

import (
	"fmt"
	"strings"
	"time"
)

const suspiciousSampleLimit = 50

var trackedLocalModelFamilies = map[string]bool{
	"gpt-5.4": true,
	"gpt-5.5": true,
}

type modelConsistencyCounters struct {
	TotalChecked int `json:"total_checked"`
	Matched      int `json:"matched"`
	Mismatched   int `json:"mismatched"`
	Unknown      int `json:"unknown"`
}

type modelFamilyAnomalyCounters struct {
	LowContextFamilyCount int `json:"low_context_family_count"`
}

type singleRequestAnomalyCounters struct {
	ModelDriftCount       int `json:"model_drift_count"`
	FingerprintDriftCount int `json:"fingerprint_drift_count"`
	RebuildSuspectedCount int `json:"rebuild_suspected_count"`
}

type familyBreakdownEntry struct {
	Consistency            modelConsistencyCounters     `json:"consistency"`
	Anomalies              modelFamilyAnomalyCounters   `json:"anomalies"`
	SingleRequestAnomalies singleRequestAnomalyCounters `json:"single_request_anomalies"`
}

type suspiciousModelSample struct {
	TS                    string     `json:"ts"`
	Path                  string     `json:"path"`
	LocalConfigModel      string     `json:"local_config_model,omitempty"`
	LocalRequestModel     string     `json:"local_request_model,omitempty"`
	EffectiveLocalModel   string     `json:"effective_local_model,omitempty"`
	UpstreamModel         string     `json:"upstream_model,omitempty"`
	StreamModel           string     `json:"stream_model,omitempty"`
	FirstObservedModel    string     `json:"first_observed_model,omitempty"`
	LastObservedModel     string     `json:"last_observed_model,omitempty"`
	ObservedModels        []string   `json:"observed_models"`
	ObservedModelFamilies []string   `json:"observed_model_families"`
	SystemFingerprint     string     `json:"system_fingerprint,omitempty"`
	ObservedFingerprints  []string   `json:"observed_fingerprints"`
	ServiceTier           string     `json:"service_tier,omitempty"`
	AnomalyType           string     `json:"anomaly_type"`
	Confidence            string     `json:"confidence"`
	EvidenceLogs          []logEntry `json:"evidence_logs"`
}

type requestModelContext struct {
	LocalConfigModel      string
	LocalRequestModel     string
	EffectiveLocalModel   string
	UpstreamModel         string
	StreamModel           string
	FinalResponseModel    string
	ServiceTier           string
	SystemFingerprint     string
	ResponseID            string
	FirstObservedModel    string
	LastObservedModel     string
	ObservedModels        map[string]bool
	ObservedModelFamilies map[string]bool
	ObservedFingerprints  map[string]bool
	ObservedResponseIDs   map[string]bool
	Finalized             bool
}

func createTrackedFamilyBreakdown() map[string]*familyBreakdownEntry {
	result := map[string]*familyBreakdownEntry{}
	for family := range trackedLocalModelFamilies {
		result[family] = &familyBreakdownEntry{}
	}
	return result
}

func calculateConsistencyMatchRatio(consistency modelConsistencyCounters) float64 {
	declaredChecked := consistency.Matched + consistency.Mismatched
	if declaredChecked == 0 {
		return 0
	}
	return float64(consistency.Matched) / float64(declaredChecked)
}

func getFamilyBreakdownEntry(mon *monitor, family string) *familyBreakdownEntry {
	if !trackedLocalModelFamilies[family] {
		return nil
	}
	if mon.FamilyBreakdown == nil {
		mon.FamilyBreakdown = createTrackedFamilyBreakdown()
	}
	if mon.FamilyBreakdown[family] == nil {
		mon.FamilyBreakdown[family] = &familyBreakdownEntry{}
	}
	return mon.FamilyBreakdown[family]
}

func incrementStringCount(counter map[string]int, value string) {
	key := strings.TrimSpace(value)
	if key == "" {
		return
	}
	counter[key]++
}

func createRequestModelContext(localConfigModel string, requestModel string) *requestModelContext {
	effectiveLocalModel := firstNonEmpty(requestModel, localConfigModel)
	return &requestModelContext{
		LocalConfigModel:      strings.TrimSpace(localConfigModel),
		LocalRequestModel:     strings.TrimSpace(requestModel),
		EffectiveLocalModel:   strings.TrimSpace(effectiveLocalModel),
		ObservedModels:        map[string]bool{},
		ObservedModelFamilies: map[string]bool{},
		ObservedFingerprints:  map[string]bool{},
		ObservedResponseIDs:   map[string]bool{},
	}
}

func recordObservedModel(context *requestModelContext, modelName string) {
	if context == nil || strings.TrimSpace(modelName) == "" {
		return
	}
	normalized := strings.TrimSpace(modelName)
	context.ObservedModels[normalized] = true
	context.ObservedModelFamilies[normalizeModelFamily(normalized)] = true
	if context.FirstObservedModel == "" {
		context.FirstObservedModel = normalized
	}
	context.LastObservedModel = normalized
}

func recordObservedFingerprint(context *requestModelContext, fingerprint string) {
	if context == nil || strings.TrimSpace(fingerprint) == "" {
		return
	}
	normalized := strings.TrimSpace(fingerprint)
	context.ObservedFingerprints[normalized] = true
	context.SystemFingerprint = normalized
}

func recordObservedResponseID(context *requestModelContext, responseID string) {
	if context == nil || strings.TrimSpace(responseID) == "" {
		return
	}
	normalized := strings.TrimSpace(responseID)
	context.ObservedResponseIDs[normalized] = true
	context.ResponseID = normalized
}

func extractPayloadModels(payload map[string]any) []string {
	models := make([]string, 0, 2)
	seen := map[string]bool{}
	add := func(value any) {
		text := strings.TrimSpace(anyToString(value))
		if text == "" || seen[text] {
			return
		}
		seen[text] = true
		models = append(models, text)
	}
	if payload != nil {
		add(payload["model"])
		if response, ok := payload["response"].(map[string]any); ok {
			add(response["model"])
		}
	}
	return models
}

func extractPayloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value := strings.TrimSpace(anyToString(payload[key]))
	if value != "" {
		return value
	}
	if response, ok := payload["response"].(map[string]any); ok {
		return strings.TrimSpace(anyToString(response[key]))
	}
	return ""
}

func extractPayloadResponseID(payload map[string]any, allowTopLevelID bool) string {
	if payload == nil {
		return ""
	}
	if allowTopLevelID {
		if value := strings.TrimSpace(anyToString(payload["id"])); value != "" {
			return value
		}
	}
	if response, ok := payload["response"].(map[string]any); ok {
		return strings.TrimSpace(anyToString(response["id"]))
	}
	return ""
}

func applyPayloadModelSignals(context *requestModelContext, payload map[string]any, fromStream bool, fromFinalResponse bool) {
	if context == nil || payload == nil {
		return
	}
	models := extractPayloadModels(payload)
	for _, modelName := range models {
		recordObservedModel(context, modelName)
	}
	if fingerprint := extractPayloadString(payload, "system_fingerprint"); fingerprint != "" {
		recordObservedFingerprint(context, fingerprint)
	}
	if serviceTier := extractPayloadString(payload, "service_tier"); serviceTier != "" {
		context.ServiceTier = serviceTier
	}
	if responseID := extractPayloadResponseID(payload, !fromStream); responseID != "" {
		recordObservedResponseID(context, responseID)
	}
	if len(models) > 0 {
		last := models[len(models)-1]
		if fromStream {
			context.StreamModel = last
		}
		if fromFinalResponse {
			context.FinalResponseModel = last
		}
		if !fromStream {
			context.UpstreamModel = last
		}
	}
}

func sortedStringSet(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	for left := 0; left < len(result); left++ {
		for right := left + 1; right < len(result); right++ {
			if result[right] < result[left] {
				result[left], result[right] = result[right], result[left]
			}
		}
	}
	return result
}

func collectSuspiciousSampleEvidenceLogs(mon *monitor, pathname string, context *requestModelContext, anomalyType string, confidence string) []logEntry {
	entries := make([]logEntry, 0, 5)
	for index := len(mon.LogEntries) - 1; index >= 0 && len(entries) < 4; index-- {
		entry := mon.LogEntries[index]
		if strings.Contains(entry.Message, "path="+pathname) {
			entries = append([]logEntry{entry}, entries...)
		}
	}
	entries = append(entries, logEntry{
		At: time.Now().Format(time.RFC3339),
		Message: fmt.Sprintf("[sample] path=%s anomaly=%s confidence=%s local=%s upstream=%s stream=%s first=%s last=%s models=%s fingerprints=%s",
			pathname,
			anomalyType,
			confidence,
			firstNonEmpty(context.EffectiveLocalModel, "-"),
			firstNonEmpty(context.UpstreamModel, "-"),
			firstNonEmpty(context.StreamModel, "-"),
			firstNonEmpty(context.FirstObservedModel, "-"),
			firstNonEmpty(context.LastObservedModel, "-"),
			firstNonEmpty(strings.Join(sortedStringSet(context.ObservedModels), "|"), "-"),
			firstNonEmpty(strings.Join(sortedStringSet(context.ObservedFingerprints), "|"), "-"),
		),
	})
	return entries
}

func pushSuspiciousModelSample(mon *monitor, pathname string, context *requestModelContext, anomalyType string, confidence string) {
	if mon == nil || context == nil {
		return
	}
	sample := suspiciousModelSample{
		TS:                    time.Now().Format(time.RFC3339),
		Path:                  pathname,
		LocalConfigModel:      context.LocalConfigModel,
		LocalRequestModel:     context.LocalRequestModel,
		EffectiveLocalModel:   context.EffectiveLocalModel,
		UpstreamModel:         context.UpstreamModel,
		StreamModel:           context.StreamModel,
		FirstObservedModel:    context.FirstObservedModel,
		LastObservedModel:     context.LastObservedModel,
		ObservedModels:        sortedStringSet(context.ObservedModels),
		ObservedModelFamilies: sortedStringSet(context.ObservedModelFamilies),
		SystemFingerprint:     context.SystemFingerprint,
		ObservedFingerprints:  sortedStringSet(context.ObservedFingerprints),
		ServiceTier:           context.ServiceTier,
		AnomalyType:           anomalyType,
		Confidence:            confidence,
		EvidenceLogs:          collectSuspiciousSampleEvidenceLogs(mon, pathname, context, anomalyType, confidence),
	}
	mon.SuspiciousModelSamples = append([]suspiciousModelSample{sample}, mon.SuspiciousModelSamples...)
	if len(mon.SuspiciousModelSamples) > suspiciousSampleLimit {
		mon.SuspiciousModelSamples = mon.SuspiciousModelSamples[:suspiciousSampleLimit]
	}
}

func looksLikeLowContextFamilyError(payload map[string]any) bool {
	if payload == nil {
		return false
	}
	text := strings.ToLower(jsonMarshal(payload))
	return strings.Contains(text, "selected model is at capacity") &&
		strings.Contains(text, "try a different model")
}

func finalizeModelInsights(mon *monitor, pathname string, context *requestModelContext, errorPayload map[string]any) {
	if mon == nil || context == nil || context.Finalized {
		return
	}
	context.Finalized = true
	mon.mu.Lock()
	defer mon.mu.Unlock()

	effectiveFamily := normalizeModelFamily(context.EffectiveLocalModel)
	familyBreakdown := getFamilyBreakdownEntry(mon, effectiveFamily)
	if context.EffectiveLocalModel != "" {
		incrementStringCount(mon.LocalModelCounts, context.EffectiveLocalModel)
	}
	if context.UpstreamModel != "" {
		incrementStringCount(mon.UpstreamModelCounts, context.UpstreamModel)
	}
	if context.StreamModel != "" {
		incrementStringCount(mon.StreamModelCounts, context.StreamModel)
	}

	if trackedLocalModelFamilies[effectiveFamily] {
		mon.ModelConsistency.TotalChecked++
		if familyBreakdown != nil {
			familyBreakdown.Consistency.TotalChecked++
		}
		declaredModel := firstNonEmpty(context.UpstreamModel, context.StreamModel, context.FinalResponseModel)
		declaredFamily := normalizeModelFamily(declaredModel)
		switch {
		case declaredFamily == "unknown":
			mon.ModelConsistency.Unknown++
			if familyBreakdown != nil {
				familyBreakdown.Consistency.Unknown++
			}
		case declaredFamily == effectiveFamily:
			mon.ModelConsistency.Matched++
			if familyBreakdown != nil {
				familyBreakdown.Consistency.Matched++
			}
		default:
			mon.ModelConsistency.Mismatched++
			if familyBreakdown != nil {
				familyBreakdown.Consistency.Mismatched++
			}
			pushSuspiciousModelSample(mon, pathname, context, "model_family_mismatch", "high")
		}
	}

	if looksLikeLowContextFamilyError(errorPayload) {
		mon.ModelFamilyAnomalies.LowContextFamilyCount++
		if familyBreakdown != nil {
			familyBreakdown.Anomalies.LowContextFamilyCount++
		}
		pushSuspiciousModelSample(mon, pathname, context, "low_context_family_behavior", "high")
	}

	switch {
	case len(context.ObservedModelFamilies) > 1:
		mon.SingleRequestAnomalies.ModelDriftCount++
		if familyBreakdown != nil {
			familyBreakdown.SingleRequestAnomalies.ModelDriftCount++
		}
		pushSuspiciousModelSample(mon, pathname, context, "single_request_model_drift", "high")
	case len(context.ObservedFingerprints) > 1:
		mon.SingleRequestAnomalies.FingerprintDriftCount++
		mon.SingleRequestAnomalies.RebuildSuspectedCount++
		if familyBreakdown != nil {
			familyBreakdown.SingleRequestAnomalies.FingerprintDriftCount++
			familyBreakdown.SingleRequestAnomalies.RebuildSuspectedCount++
		}
		pushSuspiciousModelSample(mon, pathname, context, "single_request_rebuild_suspected", "high")
	case context.FinalResponseModel != "" && context.StreamModel != "" && normalizeModelFamily(context.FinalResponseModel) != normalizeModelFamily(context.StreamModel):
		mon.SingleRequestAnomalies.RebuildSuspectedCount++
		if familyBreakdown != nil {
			familyBreakdown.SingleRequestAnomalies.RebuildSuspectedCount++
		}
		pushSuspiciousModelSample(mon, pathname, context, "single_request_rebuild_suspected", "high")
	case len(context.ObservedResponseIDs) > 1:
		mon.SingleRequestAnomalies.RebuildSuspectedCount++
		if familyBreakdown != nil {
			familyBreakdown.SingleRequestAnomalies.RebuildSuspectedCount++
		}
		pushSuspiciousModelSample(mon, pathname, context, "single_request_rebuild_suspected", "high")
	}
}

func buildModelInsightsSnapshot(runtime *appRuntime) map[string]any {
	runtime.Monitor.mu.Lock()
	defer runtime.Monitor.mu.Unlock()

	familyBreakdown := map[string]any{}
	for family := range trackedLocalModelFamilies {
		bucket := runtime.Monitor.FamilyBreakdown[family]
		if bucket == nil {
			bucket = &familyBreakdownEntry{}
		}
		familyBreakdown[family] = map[string]any{
			"consistency": map[string]any{
				"total_checked": bucket.Consistency.TotalChecked,
				"matched":       bucket.Consistency.Matched,
				"mismatched":    bucket.Consistency.Mismatched,
				"unknown":       bucket.Consistency.Unknown,
				"match_ratio":   calculateConsistencyMatchRatio(bucket.Consistency),
			},
			"anomalies":                bucket.Anomalies,
			"single_request_anomalies": bucket.SingleRequestAnomalies,
		}
	}
	suspicious := append([]suspiciousModelSample{}, runtime.Monitor.SuspiciousModelSamples...)
	return map[string]any{
		"local_config_model":    nil,
		"local_config_family":   normalizeModelFamily(""),
		"local_model_counts":    cloneCounter(runtime.Monitor.LocalModelCounts),
		"upstream_model_counts": cloneCounter(runtime.Monitor.UpstreamModelCounts),
		"stream_model_counts":   cloneCounter(runtime.Monitor.StreamModelCounts),
		"consistency": map[string]any{
			"total_checked": runtime.Monitor.ModelConsistency.TotalChecked,
			"matched":       runtime.Monitor.ModelConsistency.Matched,
			"mismatched":    runtime.Monitor.ModelConsistency.Mismatched,
			"unknown":       runtime.Monitor.ModelConsistency.Unknown,
			"match_ratio":   calculateConsistencyMatchRatio(runtime.Monitor.ModelConsistency),
		},
		"anomalies":                runtime.Monitor.ModelFamilyAnomalies,
		"single_request_anomalies": runtime.Monitor.SingleRequestAnomalies,
		"family_breakdown":         familyBreakdown,
		"suspicious_samples":       suspicious,
	}
}

func applyModelContextToReasoningSample(sample *reasoningSample, context *requestModelContext) {
	if sample == nil || context == nil {
		return
	}
	sample.EffectiveLocalModel = context.EffectiveLocalModel
	sample.EffectiveLocalModelFamily = normalizeModelFamily(context.EffectiveLocalModel)
	sample.UpstreamModel = context.UpstreamModel
	sample.StreamModel = context.StreamModel
	sample.FinalResponseModel = context.FinalResponseModel
	sample.SystemFingerprint = context.SystemFingerprint
	sample.ServiceTier = context.ServiceTier
}
