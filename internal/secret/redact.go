package secret

import (
	"os"
	"regexp"
	"sort"
	"strings"
)

const redacted = "<redacted>"

var (
	sensitiveEnvKeys = []string{
		"TELEGRAM_BOT_TOKEN",
		"TELEGRAM_API_HASH",
		"OBS_PASSWORD",
	}
	telegramBotURLTokenPattern = regexp.MustCompile(`(/bot)[^/\s"']+`)
	telegramTokenPattern       = regexp.MustCompile(`\b[0-9]{5,}:[A-Za-z0-9_-]{10,}\b`)
)

type redactedError struct {
	err error
	msg string
}

func (e redactedError) Error() string {
	return e.msg
}

func (e redactedError) Unwrap() error {
	return e.err
}

func RedactString(value string, secretValues ...string) string {
	if value == "" {
		return value
	}
	redactedValue := telegramBotURLTokenPattern.ReplaceAllString(value, `${1}`+redacted)
	redactedValue = telegramTokenPattern.ReplaceAllString(redactedValue, redacted)
	for _, secretValue := range normalizedSecretValues(secretValues) {
		redactedValue = strings.ReplaceAll(redactedValue, secretValue, redacted)
	}
	return redactedValue
}

func RedactError(err error, secretValues ...string) error {
	if err == nil {
		return nil
	}
	msg := RedactString(err.Error(), secretValues...)
	if msg == err.Error() {
		return err
	}
	return redactedError{err: err, msg: msg}
}

func ContainsSecret(value string, secretValues ...string) bool {
	if value == "" {
		return false
	}
	return RedactString(value, secretValues...) != value
}

func normalizedSecretValues(secretValues []string) []string {
	seen := make(map[string]struct{})
	values := make([]string, 0, len(secretValues)+len(sensitiveEnvKeys))
	add := func(value string) {
		if value == "" || isPlaceholder(value) {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	for _, key := range sensitiveEnvKeys {
		add(os.Getenv(key))
	}
	for _, value := range secretValues {
		add(value)
	}
	sort.Slice(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	return values
}

func isPlaceholder(value string) bool {
	return strings.HasPrefix(value, "replace-with-")
}
