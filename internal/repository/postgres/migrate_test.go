package postgres

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/sentiae/runtime-service/migrations"
)

// TestLatestMigrationVersion_IsTheNewestEmbeddedFile pins the version a
// non-migrating boot demands to the newest NNNN_*.up.sql this binary embeds.
// Computed independently here from the file names, so an iteration bug in
// LatestMigrationVersion (stopping at First, or one short of the end) makes a
// fleet host demand a schema that is not the one it was built against.
//
// Control: return src.First()'s version without walking Next ⇒ red (1 ≠ newest).
func TestLatestMigrationVersion_IsTheNewestEmbeddedFile(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	var want uint64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		n, err := strconv.ParseUint(name[:strings.IndexByte(name, '_')], 10, 64)
		if err != nil {
			t.Fatalf("migration %q has no numeric prefix: %v", name, err)
		}
		if n > want {
			want = n
		}
	}
	if want < 2 {
		t.Fatalf("found newest embedded version %d; the walk is only tested with at least two migrations", want)
	}

	got, err := LatestMigrationVersion()
	if err != nil {
		t.Fatalf("LatestMigrationVersion: %v", err)
	}
	if uint64(got) != want {
		t.Fatalf("LatestMigrationVersion() = %d, want %d (the newest embedded *.up.sql)", got, want)
	}
}
