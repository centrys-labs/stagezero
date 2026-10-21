package db

import (
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
)

func migrationFiles(t *testing.T) fstest.MapFS {
	t.Helper()
	files := fstest.MapFS{}
	err := fs.WalkDir(migrations, "migrations", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			data, err := fs.ReadFile(migrations, path)
			if err != nil {
				return err
			}
			files[path] = &fstest.MapFile{Data: data}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestMigrationBundle(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		change     func(fstest.MapFS)
	}{
		{"valid", "", nil},
		{"missing alert migration", "missing", func(files fstest.MapFS) { delete(files, "migrations/0028_alerts.sql") }},
		{"modified SQL", "digest", func(files fstest.MapFS) {
			files["migrations/0028_alerts.sql"] = &fstest.MapFile{Data: []byte("-- changed\n")}
		}},
		{"modified manifest", "manifest digest", func(files fstest.MapFS) { files["migrations/manifest-0031.json"] = &fstest.MapFile{Data: []byte(`[]`)} }},
		{"unexpected migration", "unexpected", func(files fstest.MapFS) {
			files["migrations/0032_extra.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\n")}
		}},
		{"duplicate version", "unexpected", func(files fstest.MapFS) {
			files["migrations/0028_other.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\n")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := migrationFiles(t)
			if tc.change != nil {
				tc.change(files)
			}
			err := checkMigrationBundle(files)
			if tc.want == "" && err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}
