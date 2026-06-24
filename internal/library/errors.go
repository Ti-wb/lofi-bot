package library

import (
	"errors"
	"fmt"
	"strings"
)

// ErrorCode is a stable, app-facing reason for a library operation failure.
type ErrorCode string

const (
	ErrorUnsupportedExtension ErrorCode = "unsupported_extension"
	ErrorInvalidFilename      ErrorCode = "invalid_filename"
	ErrorInvalidPeriod        ErrorCode = "invalid_period"
	ErrorReadDirectory        ErrorCode = "read_directory"
	ErrorNoCandidates         ErrorCode = "no_candidates"
	ErrorInvalidRandom        ErrorCode = "invalid_random"
)

// Error is a structured error that callers can surface without parsing text.
type Error struct {
	Code  ErrorCode
	Kind  Kind
	Path  string
	Field string
	Value string
	Err   error
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	var parts []string
	if e.Kind != "" {
		parts = append(parts, string(e.Kind))
	}
	if e.Path != "" {
		parts = append(parts, e.Path)
	}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	prefix := string(e.Code)
	if len(parts) > 0 {
		prefix += " (" + strings.Join(parts, ", ") + ")"
	}
	if e.Err != nil {
		return prefix + ": " + e.Err.Error()
	}
	if e.Value != "" {
		return prefix + ": " + e.Value
	}
	return prefix
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func fileError(code ErrorCode, kind Kind, path, field, value string, err error) *Error {
	return &Error{
		Code:  code,
		Kind:  kind,
		Path:  path,
		Field: field,
		Value: value,
		Err:   err,
	}
}

func noCandidates(kind Kind, field, value string) *Error {
	message := "no matching assets"
	if value != "" {
		message = fmt.Sprintf("no matching assets for %s %q", field, value)
	}
	return fileError(ErrorNoCandidates, kind, "", field, value, errors.New(message))
}

// ScanError groups non-fatal scan issues. Valid assets found before or after
// these issues are still returned by Scan.
type ScanError struct {
	Issues []*Error
}

func (e *ScanError) Error() string {
	if e == nil || len(e.Issues) == 0 {
		return ""
	}
	if len(e.Issues) == 1 {
		return "media library scan found 1 issue: " + e.Issues[0].Error()
	}
	return fmt.Sprintf("media library scan found %d issues", len(e.Issues))
}

func (e *ScanError) Unwrap() []error {
	if e == nil {
		return nil
	}
	errs := make([]error, 0, len(e.Issues))
	for _, issue := range e.Issues {
		if issue != nil {
			errs = append(errs, issue)
		}
	}
	return errs
}

func scanErr(issues []*Error) error {
	if len(issues) == 0 {
		return nil
	}
	return &ScanError{Issues: issues}
}
