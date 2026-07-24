//go:build !linux

package main

// The guest-agent only ever runs inside a Linux Firecracker guest; mounting is a
// linux-only syscall. This stub keeps `go build ./cmd/...` working on developer
// machines (darwin) — the real mount lives in mount_linux.go. Same shape as
// cmd/image-init/main_other.go.
func mountRunTmpfs() {}
