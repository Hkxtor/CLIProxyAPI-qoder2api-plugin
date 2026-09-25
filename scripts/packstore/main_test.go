package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readZipEntries 读取 zip 内的条目（名字 → 内容 + 模式）。
func readZipEntries(t *testing.T, path string) map[string]struct {
	data []byte
	mode os.FileMode
} {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("打开 zip %s 失败: %v", path, err)
	}
	defer func() { _ = reader.Close() }()

	entries := map[string]struct {
		data []byte
		mode os.FileMode
	}{}
	for _, file := range reader.File {
		handle, err := file.Open()
		if err != nil {
			t.Fatalf("打开条目 %s 失败: %v", file.Name, err)
		}
		data, err := io.ReadAll(handle)
		_ = handle.Close()
		if err != nil {
			t.Fatalf("读取条目 %s 失败: %v", file.Name, err)
		}
		entries[file.Name] = struct {
			data []byte
			mode os.FileMode
		}{data: data, mode: file.Mode()}
	}
	return entries
}

// TestPackArchiveMatchesHostContract 锁死宿主 pluginstore 的归档契约：
// 文件名 {id}_{version}_{goos}_{goarch}.zip、根目录下唯一动态库 {id}{ext}、可执行模式。
func TestPackArchiveMatchesHostContract(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "qoder2api-linux-amd64.so")
	payload := []byte("\x7fELF fake plugin payload")
	if err := os.WriteFile(libPath, payload, 0o755); err != nil {
		t.Fatal(err)
	}

	archivePath, err := packArchive(libPath, "qoder2api", "0.1.1", "linux", "amd64", dir)
	if err != nil {
		t.Fatalf("packArchive 失败: %v", err)
	}
	if got, want := filepath.Base(archivePath), "qoder2api_0.1.1_linux_amd64.zip"; got != want {
		t.Fatalf("归档名 = %q, want %q", got, want)
	}

	entries := readZipEntries(t, archivePath)
	if len(entries) != 1 {
		t.Fatalf("zip 内条目数 = %d, want 1（宿主拒绝含多个动态库的 zip）: %v", len(entries), entries)
	}
	entry, ok := entries["qoder2api.so"]
	if !ok {
		t.Fatalf("缺少根目录下的 qoder2api.so，实际条目: %v", entries)
	}
	if !bytes.Equal(entry.data, payload) {
		t.Fatal("zip 内动态库内容与输入不一致")
	}
	if entry.mode.Perm() != 0o755 {
		t.Fatalf("条目模式 = %v, want 0755", entry.mode.Perm())
	}
}

// TestPackArchivePlatformExtensions 校验各平台的库扩展名与归档名。
func TestPackArchivePlatformExtensions(t *testing.T) {
	cases := []struct {
		goos     string
		goarch   string
		libName  string
		entry    string
		expected string
	}{
		{"linux", "arm64", "qoder2api-linux-arm64.so", "qoder2api.so", "qoder2api_0.1.1_linux_arm64.zip"},
		{"darwin", "arm64", "qoder2api-darwin-arm64.dylib", "qoder2api.dylib", "qoder2api_0.1.1_darwin_arm64.zip"},
		{"windows", "amd64", "qoder2api-windows-amd64.dll", "qoder2api.dll", "qoder2api_0.1.1_windows_amd64.zip"},
	}
	for _, testCase := range cases {
		dir := t.TempDir()
		libPath := filepath.Join(dir, testCase.libName)
		if err := os.WriteFile(libPath, []byte("payload"), 0o755); err != nil {
			t.Fatal(err)
		}
		archivePath, err := packArchive(libPath, "qoder2api", "0.1.1", testCase.goos, testCase.goarch, dir)
		if err != nil {
			t.Fatalf("%s: packArchive 失败: %v", testCase.goos, err)
		}
		if got := filepath.Base(archivePath); got != testCase.expected {
			t.Fatalf("%s: 归档名 = %q, want %q", testCase.goos, got, testCase.expected)
		}
		entries := readZipEntries(t, archivePath)
		if _, ok := entries[testCase.entry]; !ok {
			t.Fatalf("%s: 缺少条目 %s，实际: %v", testCase.goos, testCase.entry, entries)
		}
	}
}

// TestPackArchiveStripsLeadingV 版本号带前导 v 时归档名必须去掉它（宿主按 tag 去 v 推导）。
func TestPackArchiveStripsLeadingV(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "qoder2api-linux-amd64.so")
	if err := os.WriteFile(libPath, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	archivePath, err := packArchive(libPath, "qoder2api", "v0.1.1", "linux", "amd64", dir)
	if err != nil {
		t.Fatalf("packArchive 失败: %v", err)
	}
	if got, want := filepath.Base(archivePath), "qoder2api_0.1.1_linux_amd64.zip"; got != want {
		t.Fatalf("归档名 = %q, want %q", got, want)
	}
}

// TestPackArchiveRejectsExtensionMismatch 平台与库扩展名不匹配时必须报错，
// 否则宿主会在安装阶段才发现（zip 内容与目标平台不符）。
func TestPackArchiveRejectsExtensionMismatch(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "qoder2api-linux-amd64.so")
	if err := os.WriteFile(libPath, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := packArchive(libPath, "qoder2api", "0.1.1", "windows", "amd64", dir); err == nil {
		t.Fatal("GOOS=windows 配 .so 应当报错")
	}
	if _, err := packArchive(libPath, "qoder2api", "0.1.1", "plan9", "amd64", dir); err == nil {
		t.Fatal("不支持的 GOOS 应当报错")
	}
}

// TestWriteChecksumsFile 校验和资产必须是宿主认识的格式：每行 `<64位hex>  <文件名>`。
func TestWriteChecksumsFile(t *testing.T) {
	dir := t.TempDir()
	archives := map[string][]byte{
		"qoder2api_0.1.1_linux_amd64.zip":  []byte("linux archive"),
		"qoder2api_0.1.1_darwin_arm64.zip": []byte("darwin archive"),
	}
	for name, payload := range archives {
		if err := os.WriteFile(filepath.Join(dir, name), payload, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 非 zip 文件必须被忽略
	if err := os.WriteFile(filepath.Join(dir, "qoder2api-linux-amd64.so"), []byte("raw"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := writeChecksumsFile(dir, &stdout); err != nil {
		t.Fatalf("writeChecksumsFile 失败: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(dir, "checksums.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	if len(lines) != len(archives) {
		t.Fatalf("checksums.txt 行数 = %d, want %d: %q", len(lines), len(archives), payload)
	}
	// 排序保证内容稳定（可复现）。
	if !strings.HasPrefix(lines[0], "") || !strings.Contains(lines[0], "qoder2api_0.1.1_darwin_arm64.zip") {
		t.Fatalf("checksums.txt 未按名字排序: %q", lines)
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("行格式不是 `<hash>  <name>`: %q", line)
		}
		if len(fields[0]) != sha256.Size*2 {
			t.Fatalf("sha256 长度不对: %q", fields[0])
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			t.Fatalf("sha256 不是合法 hex: %q", fields[0])
		}
		expected := sha256.Sum256(archives[fields[1]])
		if want := hex.EncodeToString(expected[:]); want != fields[0] {
			t.Fatalf("%s 的 sha256 不符：got %s, want %s", fields[1], fields[0], want)
		}
	}
}

func TestWriteChecksumsFileWithoutArchives(t *testing.T) {
	dir := t.TempDir()
	if err := writeChecksumsFile(dir, io.Discard); err == nil {
		t.Fatal("目录里没有 zip 时应当报错，而不是产出空 checksums.txt")
	}
}

// TestRunRejectsMixedModes 打包与汇总两种模式不能混用，避免误产出不完整资产。
func TestRunRejectsMixedModes(t *testing.T) {
	dir := t.TempDir()
	libPath := filepath.Join(dir, "qoder2api-linux-amd64.so")
	if err := os.WriteFile(libPath, []byte("payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"-lib", libPath, "-version", "0.1.1", "-goos", "linux", "-goarch", "amd64",
		"-out-dir", dir, "-checksums-dir", dir}, io.Discard)
	if err == nil {
		t.Fatal("同时给 -lib 与 -checksums-dir 应当报错")
	}
}

// TestRunChecksumsMode 走一遍汇总模式的命令行路径。
func TestRunChecksumsMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "qoder2api_0.1.1_linux_amd64.zip"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := run([]string{"-checksums-dir", dir}, &stdout); err != nil {
		t.Fatalf("run 失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "checksums.txt")); err != nil {
		t.Fatalf("checksums.txt 未生成: %v", err)
	}
	if !strings.Contains(stdout.String(), "checksums.txt") {
		t.Fatalf("输出未提及 checksums.txt: %q", stdout.String())
	}
}
