package cmd

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates a file holding content and returns its path.
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", p, err)
	}
	return p
}

func assertGone(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Lstat(p); !os.IsNotExist(err) {
		t.Errorf("Lstat(%s) error = %v, want it gone", p, err)
	}
}

func TestRenameNoReplaceMovesToAFreeName(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src", "new")
	dest := filepath.Join(dir, "dest")

	if err := renameNoReplace(src, dest); err != nil {
		t.Fatalf("renameNoReplace() error = %v", err)
	}
	assertFileBytes(t, dest, []byte("new"))
	assertGone(t, src)
}

func TestRenameNoReplaceRefusesAnExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "src", "new")
	dest := writeFile(t, dir, "dest", "old")

	if err := renameNoReplace(src, dest); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("renameNoReplace() error = %v, want fs.ErrExist", err)
	}
	assertFileBytes(t, dest, []byte("old"))
	assertFileBytes(t, src, []byte("new"))
}

// unsupportedMove stands in for a filesystem with no no-replace move.
func unsupportedMove(string, string) error { return errors.ErrUnsupported }

func TestPublishFile(t *testing.T) {
	for _, tt := range []struct {
		name string
		move func(oldpath, newpath string) error
	}{
		{name: "no-replace move", move: renameNoReplace},
		{name: "exclusive copy fallback", move: unsupportedMove},
	} {
		t.Run(tt.name+" to a free name", func(t *testing.T) {
			dir := t.TempDir()
			tmp := writeFile(t, dir, "tmp", "bytes")
			dest := filepath.Join(dir, "dest")

			if err := publishFile(tmp, dest, tt.move); err != nil {
				t.Fatalf("publishFile() error = %v", err)
			}
			assertFileBytes(t, dest, []byte("bytes"))
			assertGone(t, tmp)
		})

		t.Run(tt.name+" refuses an existing target", func(t *testing.T) {
			dir := t.TempDir()
			tmp := writeFile(t, dir, "tmp", "bytes")
			dest := writeFile(t, dir, "dest", "old")

			if err := publishFile(tmp, dest, tt.move); !errors.Is(err, fs.ErrExist) {
				t.Fatalf("publishFile() error = %v, want fs.ErrExist", err)
			}
			assertFileBytes(t, dest, []byte("old"))
			assertGone(t, tmp)
		})
	}
}
