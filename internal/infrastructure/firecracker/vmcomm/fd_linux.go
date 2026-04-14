//go:build linux

package vmcomm

import (
	"fmt"
	"os"
)

// fdToFile wraps a raw file descriptor in an *os.File. The caller should
// close the file after use (FileConn dups the fd internally).
func fdToFile(fd int) *os.File {
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", fd))
}
