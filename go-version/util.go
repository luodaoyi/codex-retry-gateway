package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func parseJSON(body []byte) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload
}

func parseJSONArray(body []byte) []any {
	var payload []any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload
}

func cloneJSONMap(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	body, _ := json.Marshal(source)
	var cloned map[string]any
	_ = json.Unmarshal(body, &cloned)
	return cloned
}

func ioReadAllAndClose(reader io.ReadCloser) ([]byte, error) {
	defer reader.Close()
	return io.ReadAll(reader)
}

func sanitizeRequestHeaders(headers http.Header) map[string]string {
	result := map[string]string{}
	for key, values := range headers {
		lowerKey := strings.ToLower(strings.TrimSpace(key))
		if lowerKey == "" {
			continue
		}
		switch lowerKey {
		case "authorization", "cookie", "set-cookie", "host", "content-length", "connection", "transfer-encoding":
			continue
		}
		result[lowerKey] = strings.Join(values, ", ")
	}
	return result
}

func buildRequestSummary(body []byte, headers http.Header) map[string]any {
	hash := sha256.Sum256(body)
	return map[string]any{
		"body_bytes":        len(body),
		"body_sha256":       hex.EncodeToString(hash[:]),
		"sanitized_headers": sanitizeRequestHeaders(headers),
	}
}

func truncateText(value string, maxLength int) string {
	if maxLength <= 0 || len(value) <= maxLength {
		return value
	}
	return value[:maxLength]
}

func buildRequestPayloadExcerpt(body []byte) string {
	var parsed any
	if err := json.Unmarshal(body, &parsed); err == nil {
		redacted, _ := json.Marshal(stripEncryptedContent(parsed))
		return truncateText(string(redacted), 500)
	}
	return truncateText(redactEncryptedContentText(string(body)), 500)
}

func cloneHeadersForUpstream(headers http.Header) http.Header {
	outgoing := make(http.Header)
	for key, values := range headers {
		lowerKey := strings.ToLower(key)
		switch lowerKey {
		case "host", "content-length", "connection", "transfer-encoding":
			continue
		}
		for _, value := range values {
			outgoing.Add(key, value)
		}
	}
	return outgoing
}

func anyContainsText(value any, predicate func(string) bool) bool {
	body, _ := json.Marshal(value)
	return predicate(strings.ToLower(string(body)))
}

func stripEncryptedContent(value any) any {
	switch typed := value.(type) {
	case []any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, stripEncryptedContent(item))
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "encrypted_content" {
				continue
			}
			result[key] = stripEncryptedContent(item)
		}
		return result
	default:
		return value
	}
}

func stripEncryptedContentFromBody(body []byte) []byte {
	if parsed := parseJSON(body); parsed != nil {
		redacted, _ := json.Marshal(stripEncryptedContent(parsed))
		return redacted
	}
	if bytes.Contains(bytes.ToLower(body), []byte("encrypted_content")) {
		redacted, _ := json.Marshal(map[string]any{"type": "gateway.redacted", "redacted": true})
		return redacted
	}
	return body
}
