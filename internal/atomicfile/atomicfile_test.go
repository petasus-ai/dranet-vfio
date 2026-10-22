/*
Copyright The Petasus Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteCreatesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := Write(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Errorf("contents = %q", data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, want 0644", info.Mode().Perm())
	}
}

func TestWriteReplacesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("new"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Errorf("contents = %q, want new", data)
	}
}

// A reader that lists the directory must never see the temporary file.
func TestWriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "f.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "f.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v", names)
	}
}

func TestWriteReportsAMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "f.json")
	if err := Write(path, []byte("x"), 0o644); err == nil {
		t.Fatal("expected an error when the directory does not exist")
	}
}

// The rename fails when the destination is a non-empty directory, and the
// temporary file must still be cleaned up.
func TestWriteReportsARenameFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.json")
	if err := os.MkdirAll(filepath.Join(path, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("x"), 0o644); err == nil {
		t.Fatal("expected an error when the destination cannot be replaced")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("a temporary file was left behind: %v", names)
	}
}

func TestWriteReportsAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := Write(filepath.Join(dir, "f.json"), []byte("x"), 0o644); err == nil {
		t.Fatal("expected an error when a temporary file cannot be created")
	}
}
