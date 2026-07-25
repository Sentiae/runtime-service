// Package hostcapacity reads a fleet host's PHYSICAL capacity from the machine
// itself — the filesystem that holds the volume directory, the CPU count, and the
// kernel's memory total. It exists so the fleet registry advertises measured
// numbers instead of configured ones (config narrows a measurement; it never
// substitutes for it — see domain.ResolveHostCapacity).
package hostcapacity

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/sentiae/runtime-service/internal/domain"
)

// mib is the byte→MB divisor used for every reported figure.
const mib = 1 << 20

// procMemInfo is the kernel's memory summary. A parameter of memTotalMB (not a
// literal inside it) so the parser is testable off a Linux host.
const procMemInfo = "/proc/meminfo"

// Measure reads the host's capacity. volumeDir is the directory under which
// per-volume backing files are materialized: its FILESYSTEM is the one whose
// space the fleet can actually place, so it — not the root filesystem — is what
// gets measured.
//
// Any failure is returned, never defaulted: the caller's contract is that a host
// which cannot measure itself does not register (domain.ErrHostCapacityUnmeasured).
func Measure(volumeDir string) (domain.HostCapacityMeasurement, error) {
	st, measuredDir, err := statfsNearest(volumeDir)
	if err != nil {
		return domain.HostCapacityMeasurement{}, fmt.Errorf("statfs %s: %w", measuredDir, err)
	}
	// Bsize is int64 on linux and int32 on darwin; the conversion keeps one
	// implementation for both without a build tag.
	bsize := uint64(st.Bsize)
	memMB, err := memTotalMB(procMemInfo)
	if err != nil {
		return domain.HostCapacityMeasurement{}, err
	}
	return domain.HostCapacityMeasurement{
		VCPU:            runtime.NumCPU(),
		MemTotalMB:      memMB,
		DiskTotalMB:     int64(uint64(st.Blocks) * bsize / mib),
		DiskAvailableMB: int64(uint64(st.Bavail) * bsize / mib),
	}, nil
}

// statfsNearest statfs's path, walking up to the nearest EXISTING ancestor when
// the path itself does not exist yet.
//
// The volume directory is created lazily by the first volume, so on a
// freshly-imaged host it is legitimately absent at boot — and refusing to
// register a host for that reason would keep the fleet permanently empty. Free
// space is a property of the MOUNT, so an ancestor on the same filesystem gives
// the same answer; the one case it does not is a directory that will later be a
// separate mountpoint, and there the pre-mount answer is the root filesystem's,
// which is reported as-is rather than guessed at. It returns the directory it
// actually measured so the caller can log which one that was.
func statfsNearest(path string) (syscall.Statfs_t, string, error) {
	dir := filepath.Clean(path)
	if dir == "" || dir == "." {
		dir = string(filepath.Separator)
	}
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(dir, &st)
		if err == nil {
			return st, dir, nil
		}
		parent := filepath.Dir(dir)
		if !errors.Is(err, syscall.ENOENT) || parent == dir {
			return st, dir, err
		}
		dir = parent
	}
}

// memTotalMB parses MemTotal (reported in kB) out of a /proc/meminfo-shaped file.
func memTotalMB(path string) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("%s: MemTotal line %q has no value", path, line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: parse MemTotal %q: %w", path, fields[1], err)
		}
		return kb / 1024, nil
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return 0, fmt.Errorf("%s: no MemTotal line", path)
}
