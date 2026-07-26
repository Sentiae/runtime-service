package usecase

import (
	"encoding/binary"
	"io"
	"math"
	"os"
)

// Host-side ext4 superblock probe, used by the reconciler's placement
// precondition to tell "the backing file exists" apart from "the backing file
// could actually be mounted". A present-but-unmountable ext4 fails the guest
// mount closed, which means the replica dies on every boot and the reconciler
// re-places it every tick — the same churn the backing-file-missing gate was
// written to stop, one failure mode over.
//
// ⚠ IT IS A CHURN SUPPRESSOR, NOT A DATA GUARD. The guard is the guest's mount,
// which already fails closed; this probe only decides whether the host should
// bother booting. So it fails OPEN on everything it cannot disprove: an
// unopenable file, a short read, nonsense geometry and a zero block count all
// return "mountable" and let the boot proceed. A false refusal here would strand
// a healthy app, which is strictly worse than a wasted boot.
//
// COVERAGE LIMIT, stated plainly: this reads ONE structure — the primary
// superblock at file offset 1024. A valid superblock over corrupt group
// descriptors, a wrecked journal or a shredded inode table still reads as
// mountable here, and must: the guest's refusal is the backstop and this probe
// never claims otherwise. It also never claims data is GONE — an ext4 that does
// not present a superblock is very often repairable with `e2fsck -fy`.
//
// ⚠ HEXAGONAL FORWARD REFERENCE (`#extract-a-volumeplane-port`): this file
// touches the host filesystem directly from the usecase layer, exactly as the
// os.Stat check in evalPlaceableOnBackingFile already does. Both move behind the
// VolumePlane port when it is extracted; keep them adjacent so they relocate
// together rather than one being found later.

// ext4Verdict is what the probe could establish about a backing file.
type ext4Verdict int

const (
	// ext4Mountable — the file presents a plausible ext4, OR the probe could not
	// disprove one. Placement proceeds either way (fail open).
	ext4Mountable ext4Verdict = iota
	// ext4NoSuperblock — no ext4 magic where the primary superblock must be, or
	// the file is too small to hold one at all.
	ext4NoSuperblock
	// ext4Truncated — the superblock describes a filesystem larger than the file
	// that contains it: the shape of an interrupted copy or a truncated restore.
	ext4Truncated
)

// reason returns a stable token for log attrs (never a sentence — the log
// message carries the prose).
func (v ext4Verdict) reason() string {
	switch v {
	case ext4NoSuperblock:
		return "no-ext4-superblock"
	case ext4Truncated:
		return "filesystem-larger-than-file"
	default:
		return "mountable"
	}
}

// ext4 primary superblock offsets, relative to the start of the superblock
// (which itself begins at byte 1024 of the filesystem).
const (
	ext4SuperblockOffset  = 1024
	ext4SuperblockLen     = 1024
	ext4OffBlocksCountLo  = 0x04
	ext4OffLogBlockSize   = 0x18
	ext4OffMagic          = 0x38
	ext4OffBlocksCountHi  = 0x150
	ext4Magic             = 0xEF53
	ext4MaxLogBlockSize   = 6 // 1024<<6 = 64KiB; anything larger is nonsense
	ext4MinPlausibleBytes = ext4SuperblockOffset + ext4SuperblockLen
)

// parseExt4Superblock decides the verdict from an already-read primary
// superblock. Pure: no I/O, so the decision table is directly testable. sb is
// the 1024 bytes at file offset 1024; fileBytes is the size of the whole
// backing file. The second return is the size the superblock claims for the
// filesystem, meaningful only for ext4Truncated.
func parseExt4Superblock(sb []byte, fileBytes int64) (ext4Verdict, int64) {
	if fileBytes < ext4MinPlausibleBytes || len(sb) < ext4OffBlocksCountHi+4 {
		return ext4NoSuperblock, 0
	}
	if binary.LittleEndian.Uint16(sb[ext4OffMagic:]) != ext4Magic {
		return ext4NoSuperblock, 0
	}

	logBlockSize := binary.LittleEndian.Uint32(sb[ext4OffLogBlockSize:])
	if logBlockSize > ext4MaxLogBlockSize {
		// Geometry we do not understand. Inconclusive ⇒ allow.
		return ext4Mountable, 0
	}
	blockSize := uint64(1024) << logBlockSize
	blocks := uint64(binary.LittleEndian.Uint32(sb[ext4OffBlocksCountLo:])) |
		uint64(binary.LittleEndian.Uint32(sb[ext4OffBlocksCountHi:]))<<32
	if blocks == 0 {
		// A magic-bearing superblock claiming zero blocks tells us nothing.
		return ext4Mountable, 0
	}

	// Divide rather than multiply so an absurd block count cannot overflow the
	// comparison. Exact for integers: blocks*blockSize > fileBytes ⟺
	// blocks > floor(fileBytes/blockSize). Exact equality passes — a filesystem
	// that fills its file precisely is the normal case.
	if blocks > uint64(fileBytes)/blockSize {
		return ext4Truncated, saturatingBytes(blocks, blockSize)
	}
	return ext4Mountable, int64(blocks * blockSize)
}

// saturatingBytes multiplies without wrapping, so a corrupt block count reports
// a huge number instead of a negative one in a log line.
func saturatingBytes(blocks, blockSize uint64) int64 {
	if blocks > uint64(math.MaxInt64)/blockSize {
		return math.MaxInt64
	}
	return int64(blocks * blockSize)
}

// probeExt4Mountable reads the primary superblock of path and returns the
// verdict. Any open error, read error or short read is returned as an error and
// the caller MUST treat that as inconclusive and allow placement — see the file
// header.
func probeExt4Mountable(path string, fileBytes int64) (ext4Verdict, int64, error) {
	if fileBytes < ext4MinPlausibleBytes {
		// Decided by size alone: the file cannot contain a primary superblock, so
		// there is nothing to read. This is a real verdict, not an inconclusive one.
		verdict, fsBytes := parseExt4Superblock(nil, fileBytes)
		return verdict, fsBytes, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return ext4Mountable, 0, err
	}
	defer func() { _ = f.Close() }() // read-only: a close error cannot lose data

	buf := make([]byte, ext4SuperblockLen)
	n, rerr := f.ReadAt(buf, ext4SuperblockOffset)
	if n < ext4SuperblockLen {
		if rerr == nil {
			// ReadAt must error on a short read; belt-and-braces so a partially
			// filled buffer is never parsed as evidence.
			rerr = io.ErrUnexpectedEOF
		}
		return ext4Mountable, 0, rerr
	}
	verdict, fsBytes := parseExt4Superblock(buf, fileBytes)
	return verdict, fsBytes, nil
}
