// Package buildstamp holds no code. It exists as the smallest compilable unit
// for the guard in buildstamp_test.go, which relinks this package's test binary
// under the Dockerfile's own -ldflags -X arguments and proves the injected
// build identity is really readable through platform-kit/buildinfo.
//
// The guard needs its own package because `go test -ldflags` relinks whatever
// package it is pointed at: pointing it at a heavy package would make the check
// a slow rebuild of half the service, and pointing it at a package that does
// not reference buildinfo would prove nothing.
package buildstamp
