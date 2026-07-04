package domain

import "errors"

// Compile errors. These cover the multi-file project compile path
// (RuntimeService.Compile), which builds in an ephemeral container and
// returns diagnostics without executing the result.
var (
	// ErrUnsupportedCompileLanguage is returned when the requested
	// language has no toolchain image / build command wired.
	ErrUnsupportedCompileLanguage = errors.New("unsupported compile language")

	// ErrNoSourceFiles is returned when a compile request carries no files.
	ErrNoSourceFiles = errors.New("compile request has no source files")

	// ErrCompileToolchainUnavailable is an infrastructure error: the build
	// backend (Docker CLI) is not available, so the project cannot be
	// compiled. Callers degrade rather than treating it as a user error.
	ErrCompileToolchainUnavailable = errors.New("compile toolchain unavailable")
)
