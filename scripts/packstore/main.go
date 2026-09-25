// Command packstore 生成 CLIProxyAPI 插件商店（plugin store）要求的发布资产。
//
// 宿主 internal/pluginstore 的安装契约（v7 实测）：
//
//   - 归档资产名：{id}_{version}_{goos}_{goarch}.zip
//     （version 是 tag 去掉前导 v，例如 tag v0.1.1 → 0.1.1）
//   - 校验和资产名必须**恰好**是 checksums.txt，格式为 `<sha256>  <文件名>`
//   - zip 内**根目录**必须有且仅有一个动态库，文件名必须是 {id}{ext}
//     或 {id}-v{version}{ext}；zip 内出现其它动态库会被拒绝
//   - 宿主会校验 zip 的 sha256，然后解包到 {pluginsDir}/{goos}/{goarch}/
//
// 用 Go 实现而不是 shell：`zip`/`sha256sum` 在 Windows runner 上不可用，
// 而这里必须三平台行为一致。
//
// 两种模式：
//
//	打包：  packstore -lib dist/qoder2api-linux-amd64.so -id qoder2api \
//	                 -version 0.1.1 -goos linux -goarch amd64 -out-dir dist
//	校验和：packstore -checksums-dir dist      # 汇总目录下所有 *.zip → checksums.txt
//
// 汇总步骤必须单独做：矩阵里每个平台各生成一份 checksums.txt 会互相覆盖，
// 导致部分 zip 的校验和丢失、安装时直接失败。
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "packstore:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("packstore", flag.ContinueOnError)
	flags.SetOutput(stdout)

	libPath := flags.String("lib", "", "要打包的动态库路径（打包模式必填）")
	pluginID := flags.String("id", "qoder2api", "插件 ID（必须与动态库文件名一致）")
	version := flags.String("version", "", "商店版本号，即 tag 去掉前导 v（打包模式必填）")
	goos := flags.String("goos", "", "目标 GOOS（打包模式必填）")
	goarch := flags.String("goarch", "", "目标 GOARCH（打包模式必填）")
	outDir := flags.String("out-dir", "dist", "输出目录")
	checksumsDir := flags.String("checksums-dir", "", "汇总该目录下所有 *.zip 生成 checksums.txt")

	if err := flags.Parse(args); err != nil {
		return err
	}

	if *checksumsDir != "" {
		if *libPath != "" {
			return errors.New("-checksums-dir 与打包参数不能同时使用")
		}
		return writeChecksumsFile(*checksumsDir, stdout)
	}

	if *libPath == "" || *version == "" || *goos == "" || *goarch == "" {
		return errors.New("打包模式需要 -lib、-version、-goos、-goarch")
	}

	archivePath, err := packArchive(*libPath, *pluginID, *version, *goos, *goarch, *outDir)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已生成 %s\n", archivePath)
	return nil
}

// packArchive 把动态库打包成商店要求的 zip，返回 zip 路径。
func packArchive(libPath, pluginID, version, goos, goarch, outDir string) (string, error) {
	pluginID = strings.TrimSpace(pluginID)
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if pluginID == "" || version == "" {
		return "", errors.New("插件 ID 与版本号不能为空")
	}
	if _, err := os.Stat(libPath); err != nil {
		return "", fmt.Errorf("找不到动态库 %s: %w", libPath, err)
	}
	extension, err := pluginExtension(goos)
	if err != nil {
		return "", err
	}
	entryName := pluginID + extension
	if !strings.HasSuffix(strings.ToLower(libPath), extension) {
		return "", fmt.Errorf("%s 的扩展名与 GOOS=%s 要求的 %s 不一致", libPath, goos, extension)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", err
	}

	archivePath := filepath.Join(outDir, archiveName(pluginID, version, goos, goarch))
	file, err := os.Create(archivePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()

	writer := zip.NewWriter(file)
	// zip 内条目必须位于根目录（宿主会拒绝嵌套路径），并且是可执行的普通文件。
	header := &zip.FileHeader{Name: entryName, Method: zip.Deflate}
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return "", err
	}
	source, err := os.Open(libPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.Close() }()
	if _, err := io.Copy(entry, source); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return archivePath, nil
}

// writeChecksumsFile 汇总目录下所有 zip 的 sha256 写入 checksums.txt。
func writeChecksumsFile(dir string, stdout io.Writer) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".zip") {
			continue
		}
		names = append(names, entry.Name())
	}
	if len(names) == 0 {
		return fmt.Errorf("%s 下没有找到任何 .zip", dir)
	}
	// 排序让 checksums.txt 稳定可复现。
	sort.Strings(names)

	lines := make([]string, 0, len(names))
	for _, name := range names {
		sum, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		lines = append(lines, sum+"  "+name)
	}
	target := filepath.Join(dir, "checksums.txt")
	payload := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(target, []byte(payload), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "已生成 %s：\n%s", target, payload)
	return nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// archiveName 与宿主 pluginstore.ArchiveName 保持一致。
func archiveName(pluginID, version, goos, goarch string) string {
	return fmt.Sprintf("%s_%s_%s_%s.zip", pluginID, version, goos, goarch)
}

func pluginExtension(goos string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin", "mac", "macos", "osx":
		return ".dylib", nil
	case "windows":
		return ".dll", nil
	case "linux":
		return ".so", nil
	default:
		return "", fmt.Errorf("不支持的 GOOS %q", goos)
	}
}
