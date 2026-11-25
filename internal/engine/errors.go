package engine

import (
	"errors"
	"fmt"

	"github.com/sandgorgon/polydb/internal/txn"
)

// Code is an error code, matching Rhizome's (spec/rhizome.md §14).
type Code string

const (
	CodeType         Code = "type"
	CodeConstraint   Code = "constraint"
	CodeUnknownField Code = "unknown_field"
	CodeNotFound     Code = "not_found"
	CodeConflict     Code = "conflict"
	CodeLimit        Code = "limit"
	CodeInternal     Code = "internal"
	// CodeUnsupported marks features that need a later phase, such as
	// full-text search.
	CodeUnsupported Code = "unsupported"
)

// Error is an engine error with a code.
type Error struct {
	Code Code
	Msg  string
	Err  error // the underlying error, if any
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Msg }
func (e *Error) Unwrap() error { return e.Err }

func errorf(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// wrap turns err into an *Error with the given code, unless it already is
// one. Transaction conflicts always get CodeConflict.
func wrap(code Code, err error) error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return err
	}
	if errors.Is(err, txn.ErrConflict) {
		code = CodeConflict
	}
	return &Error{Code: code, Msg: err.Error(), Err: err}
}

// CodeOf returns the code of err, or "" if err is nil. Errors that didn't
// come from the engine are CodeInternal.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if errors.Is(err, txn.ErrConflict) {
		return CodeConflict
	}
	return CodeInternal
}

// IsConflict reports whether err is a transaction conflict, which is safe
// to retry.
func IsConflict(err error) bool { return CodeOf(err) == CodeConflict }
