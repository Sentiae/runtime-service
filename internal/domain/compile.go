package domain

// SourceFile is one file in a multi-file compile request. Path is
// repo-relative and may contain subdirectories (e.g.
// "internal/domain/user.go"); the build adapter recreates the tree.
type SourceFile struct {
	Path    string
	Content string
}

// CompileDiagnostic is one structured compiler message extracted from the
// build output. Line and Column are 1-based; zero means the compiler did
// not attribute the message to a specific location.
type CompileDiagnostic struct {
	File    string
	Line    int
	Column  int
	Message string
}

// CompileResult is the outcome of compiling a project. OK is true only
// when the toolchain reported success (exit 0). RawOutput holds the
// (truncated) combined build output for callers that want the unparsed
// form. The compiled artifact is never executed.
type CompileResult struct {
	OK            bool
	Diagnostics  []CompileDiagnostic
	RawOutput     string
	CompileTimeMS int64
}
