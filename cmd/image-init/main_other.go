//go:build !linux

// image-init is the PID 1 init for a Firecracker guest and only ever runs on
// Linux. This stub exists so `go build ./cmd/...` succeeds on developer
// machines (darwin); the real init lives in main.go behind a linux build tag.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "image-init: this binary only runs on linux (Firecracker guest init)")
	os.Exit(1)
}
