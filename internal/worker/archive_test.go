package worker

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

type tarEntry struct {
	name     string
	kind     byte
	mode     int64
	body     string
	linkname string
}

func writeArchive(t *testing.T, entries []tarEntry) string {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(gzipWriter)
	for _, entry := range entries {
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: entry.kind, Linkname: entry.linkname}
		if entry.kind == tar.TypeReg {
			header.Size = int64(len(entry.body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			if _, err := writer.Write([]byte(entry.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	if err := os.WriteFile(path, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractRejectsTraversalAndLinks(t *testing.T) {
	destination := t.TempDir()
	cases := [][]tarEntry{
		{{name: "../escape.txt", kind: tar.TypeReg, mode: 0o644, body: "x"}},
		{{name: "nested/../../escape.txt", kind: tar.TypeReg, mode: 0o644, body: "x"}},
		{{name: "link", kind: tar.TypeSymlink, mode: 0o777, linkname: "/etc/passwd"}},
		{{name: "hard", kind: tar.TypeLink, mode: 0o644, linkname: "target"}},
	}
	for index, entries := range cases {
		archive := writeArchive(t, entries)
		if err := extractTarGz(archive, destination, 1<<20, 100); err == nil {
			t.Fatalf("case %d was accepted", index)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(destination), "escape.txt")); err == nil {
			t.Fatalf("case %d escaped the workspace", index)
		}
	}
}

func TestExtractRejectsDuplicateEntries(t *testing.T) {
	archive := writeArchive(t, []tarEntry{
		{name: "a.txt", kind: tar.TypeReg, mode: 0o644, body: "one"},
		{name: "a.txt", kind: tar.TypeReg, mode: 0o644, body: "two"},
	})
	if err := extractTarGz(archive, t.TempDir(), 1<<20, 100); err == nil {
		t.Fatal("duplicate entries were accepted")
	}
}

func TestExtractValidArchive(t *testing.T) {
	archive := writeArchive(t, []tarEntry{
		{name: "src", kind: tar.TypeDir, mode: 0o755},
		{name: "src/main.go", kind: tar.TypeReg, mode: 0o644, body: "package main\n"},
		{name: "src/run.sh", kind: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
	})
	destination := t.TempDir()
	if err := extractTarGz(archive, destination, 1<<20, 100); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "src", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "package main\n" {
		t.Fatalf("unexpected content %q", data)
	}
	info, err := os.Stat(filepath.Join(destination, "src", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatal("executable bit was not preserved")
	}
}

func TestExtractEnforcesLimits(t *testing.T) {
	archive := writeArchive(t, []tarEntry{{name: "large.bin", kind: tar.TypeReg, mode: 0o644, body: "0123456789"}})
	if err := extractTarGz(archive, t.TempDir(), 5, 100); err == nil {
		t.Fatal("size limit was not enforced")
	}
	archive = writeArchive(t, []tarEntry{
		{name: "a", kind: tar.TypeReg, mode: 0o644, body: "a"},
		{name: "b", kind: tar.TypeReg, mode: 0o644, body: "b"},
	})
	if err := extractTarGz(archive, t.TempDir(), 1<<20, 1); err == nil {
		t.Fatal("file count limit was not enforced")
	}
}
