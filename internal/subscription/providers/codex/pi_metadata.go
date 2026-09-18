package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Fixed typed allowlist mirrored in pi-driver/metadata.mjs. No opaque IDs,
// display names or arbitrary namespaces may cross this boundary.
const piMaxRetrySeconds = 31622400 // 366 days, including relative quota resets.
const piMaxResetAt = 253402300799  // Last second of year 9999.
const piMaxWindowMinutes = 527040

var piInteger = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)
var piDecimal = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]+)?$`)
var piQuotaWindow = regexp.MustCompile(`^x-codex-(?:bengalfox-)?(?:primary|secondary)-(used-percent|window-minutes|reset-at|reset-after-seconds)$`)
var piQuotaBoolean = regexp.MustCompile(`^x-codex-(?:bengalfox-)?(?:allowed|limit-reached)$`)
var piGenericQuota = regexp.MustCompile(`^x-codex-(?:(?:primary|secondary)-|allowed$|limit-reached$|active-limit$)`)
var piNamespacedQuota = regexp.MustCompile(`^x-codex-.+-(primary|secondary)-(used-percent|window-minutes|reset-at|reset-after-seconds)$`)

func piActiveLimit(v string) bool { return v == "premium" || v == "codex" || v == "codex_bengalfox" }
func piBoundedInteger(v string, min, max int64) (int64, bool) {
	if len(v) > 16 || !piInteger.MatchString(v) {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n >= min && n <= max
}
func piRetryAfter(raw json.RawMessage) *time.Duration {
	// The bridge emits a JSON integer, never a string or an absolute date.
	n, ok := piBoundedInteger(string(raw), 0, piMaxRetrySeconds)
	if !ok {
		return nil
	}
	duration := time.Duration(n) * time.Second
	return &duration
}
func piSafeMetadata(input map[string]string) map[string]string {
	values := make(map[string]string, len(input))
	// Normalize casing, but never resolve duplicate spellings nondeterministically.
	for k, v := range input {
		key := strings.ToLower(k)
		if _, exists := values[key]; exists {
			return nil
		}
		values[key] = v
	}
	result := map[string]string{}
	active, present := values["x-codex-active-limit"]
	unsafeGeneric := present && !piActiveLimit(active)
	for key := range values {
		if piNamespacedQuota.MatchString(key) && !piQuotaWindow.MatchString(key) {
			unsafeGeneric = true
		}
	}
	for key, v := range values {
		if unsafeGeneric && piGenericQuota.MatchString(key) {
			continue
		}
		if key == "retry-after" {
			if n, ok := piBoundedInteger(v, 0, piMaxRetrySeconds); ok {
				result[key] = strconv.FormatInt(n, 10)
			}
			continue
		}
		if key == "x-codex-active-limit" {
			if piActiveLimit(v) {
				result[key] = v
			}
			continue
		}
		if piQuotaBoolean.MatchString(key) {
			switch v {
			case "true", "1":
				result[key] = "true"
			case "false", "0":
				result[key] = "false"
			}
			continue
		}
		match := piQuotaWindow.FindStringSubmatch(key)
		if match == nil {
			continue
		}
		field := match[1]
		if field == "used-percent" {
			if len(v) <= 16 && piDecimal.MatchString(v) {
				if n, err := strconv.ParseFloat(v, 64); err == nil && n <= 100 {
					result[key] = strconv.FormatFloat(n, 'f', -1, 64)
				}
			}
			continue
		}
		max := int64(piMaxRetrySeconds)
		switch field {
		case "window-minutes":
			max = piMaxWindowMinutes
		case "reset-at":
			max = piMaxResetAt
		}
		if n, ok := piBoundedInteger(v, 1, max); ok {
			result[key] = strconv.FormatInt(n, 10)
		}
	}
	return result
}
