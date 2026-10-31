package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsXrayUnit(t *testing.T) {
	cases := []struct {
		input    string
		expected bool
	}{
		{"xray", true},
		{"xray.service", true},
		{" xray ", true},
		{" xray.service ", true},
		{"x-ui", false},
		{"x-ui.service", false},
		{"xray2", false},
		{"my-xray", false},
		{"", false},
	}

	for _, c := range cases {
		actual := isXrayUnit(c.input)
		if actual != c.expected {
			t.Errorf("isXrayUnit(%q) = %v, expected %v", c.input, actual, c.expected)
		}
	}
}

func TestExtractXrayTarGz_SecurityValidation(t *testing.T) {
	tempDir := t.TempDir()

	// 1. 测试包含恶意路径 ../evil.json
	t.Run("rejects_path_traversal", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "traversal.tar.gz")
		createTestArchive(t, archivePath, []tarEntry{
			{name: "../evil.json", data: []byte("malicious")},
		})
		destDir := filepath.Join(tempDir, "dest1")
		_ = os.MkdirAll(destDir, 0700)

		_, _, err := extractXrayTarGz(archivePath, destDir)
		if err == nil {
			t.Fatalf("expected error on path traversal, got nil")
		}
		if !strings.Contains(err.Error(), "unrecognized file") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	// 2. 测试包含目录条目
	t.Run("rejects_directory_entry", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "dir.tar.gz")
		createTestArchiveWithHeader(t, archivePath, &tar.Header{
			Name:     "config.json",
			Typeflag: tar.TypeDir,
		}, []byte("test"))
		destDir := filepath.Join(tempDir, "dest2")
		_ = os.MkdirAll(destDir, 0700)

		_, _, err := extractXrayTarGz(archivePath, destDir)
		if err == nil {
			t.Fatalf("expected error on directory entry, got nil")
		}
		if !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("unexpected error message: %v", err)
		}
	})

	// 3. 测试正常提取 config.json 与 node.env
	t.Run("extracts_valid_archive", func(t *testing.T) {
		archivePath := filepath.Join(tempDir, "valid.tar.gz")
		configData := []byte(`{"inbounds":[]}`)
		envData := []byte("PORT=443\n")
		createTestArchive(t, archivePath, []tarEntry{
			{name: "config.json", data: configData},
			{name: "node.env", data: envData},
		})
		destDir := filepath.Join(tempDir, "dest3")
		_ = os.MkdirAll(destDir, 0700)

		hasConfig, hasEnv, err := extractXrayTarGz(archivePath, destDir)
		if err != nil {
			t.Fatalf("extract failed: %v", err)
		}
		if !hasConfig || !hasEnv {
			t.Fatalf("expected hasConfig=true and hasEnv=true, got %v, %v", hasConfig, hasEnv)
		}

		readConfig, err := os.ReadFile(filepath.Join(destDir, "config.json"))
		if err != nil || string(readConfig) != string(configData) {
			t.Errorf("config.json mismatch: %s, err: %v", string(readConfig), err)
		}
		readEnv, err := os.ReadFile(filepath.Join(destDir, "node.env"))
		if err != nil || string(readEnv) != string(envData) {
			t.Errorf("node.env mismatch: %s, err: %v", string(readEnv), err)
		}
	})
}

type tarEntry struct {
	name string
	data []byte
}

func createTestArchive(t *testing.T, archivePath string, entries []tarEntry) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create archive file: %v", err)
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	for _, entry := range entries {
		header := &tar.Header{
			Name:     entry.name,
			Mode:     0600,
			Size:     int64(len(entry.data)),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("write tar header: %v", err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			t.Fatalf("write tar body: %v", err)
		}
	}
}

func createTestArchiveWithHeader(t *testing.T, archivePath string, header *tar.Header, data []byte) {
	t.Helper()
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("create archive file: %v", err)
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	if err := tw.WriteHeader(header); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if len(data) > 0 {
		_, _ = tw.Write(data)
	}
}
