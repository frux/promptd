package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteArchiveIsDeterministicAndPreservesModes(t *testing.T) {
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "promptd")
	licensePath := filepath.Join(directory, "LICENSE")
	if err := os.WriteFile(binaryPath, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(licensePath, []byte("license"), 0o644); err != nil {
		t.Fatal(err)
	}
	files := []archiveFile{
		{Name: "promptd", Path: binaryPath, Mode: 0o755},
		{Name: "LICENSE", Path: licensePath, Mode: 0o644},
	}
	sourceDate := time.Unix(1_700_000_000, 0).UTC()
	first := filepath.Join(directory, "first.tar.gz")
	second := filepath.Join(directory, "second.tar.gz")
	firstHash, err := writeArchive(first, sourceDate, files)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := writeArchive(second, sourceDate, files)
	if err != nil {
		t.Fatal(err)
	}
	firstBytes, _ := os.ReadFile(first)
	secondBytes, _ := os.ReadFile(second)
	if string(firstBytes) != string(secondBytes) || firstHash != secondHash {
		t.Fatal("archives are not reproducible")
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256(firstBytes))
	if firstHash != wantHash {
		t.Fatalf("hash = %q, want %q", firstHash, wantHash)
	}

	compressed, err := gzip.NewReader(bytes.NewReader(firstBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close()
	archive := tar.NewReader(compressed)
	for index, expected := range files {
		header, err := archive.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != expected.Name || header.Mode != expected.Mode || !header.ModTime.Equal(sourceDate) {
			t.Fatalf("header %d = %#v", index, header)
		}
		contents, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		original, _ := os.ReadFile(expected.Path)
		if string(contents) != string(original) {
			t.Fatalf("contents for %s = %q", expected.Name, contents)
		}
	}
	if _, err := archive.Next(); err != io.EOF {
		t.Fatalf("final archive.Next() error = %v", err)
	}
}

func TestWriteChecksumMatchesInstallerFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asset.sha256")
	checksum := strings.Repeat("a", 64)
	if err := writeChecksum(path, checksum, "promptd_linux_amd64.tar.gz"); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := checksum + "  promptd_linux_amd64.tar.gz\n"
	if string(contents) != want {
		t.Fatalf("checksum file = %q, want %q", contents, want)
	}
}

func TestValidateOptionsRejectsUnsafeMetadata(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"LICENSE", "README.md", "go.mod"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := Options{
		Root: root, Output: filepath.Join(root, "dist"), Version: "v1.2.3", Commit: "abc123", SourceDate: time.Now(),
	}
	invalidVersion := base
	invalidVersion.Version = "latest"
	if err := validateOptions(invalidVersion); err == nil {
		t.Fatal("invalid version accepted")
	}
	invalidCommit := base
	invalidCommit.Commit = "abc 123"
	if err := validateOptions(invalidCommit); err == nil {
		t.Fatal("invalid commit accepted")
	}
}
