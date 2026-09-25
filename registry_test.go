package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// 宿主 CLIProxyAPI/internal/pluginstore 对三方源清单的校验规则（v7 实测）：
//   - 顶层 schema_version 必须等于 1
//   - 每个条目必填 id / name / description / author；github-release 类型还要 repository
//   - id 匹配 ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$
//   - version 可选（宿主从 latest release 推导），有则必须匹配 ^[0-9][0-9A-Za-z.+-]*$
//   - repository 必须是 https://github.com/{owner}/{repo}
//
// registry.json 是用户配置三方源后真正会被读取的文件，手改坏 = 商店里直接看不见插件，
// 所以这里用测试锁住它。
type registryManifest struct {
	SchemaVersion int `json:"schema_version"`
	Plugins       []struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Author      string `json:"author"`
		Version     string `json:"version"`
		Repository  string `json:"repository"`
		Install     struct {
			Type string `json:"type"`
		} `json:"install"`
		Tags []string `json:"tags"`
	} `json:"plugins"`
}

var (
	pluginIDPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	pluginVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z.+-]*$`)
	githubRepository     = regexp.MustCompile(`^https://github\.com/[^/]+/[^/]+$`)
)

func TestRegistryManifestMatchesHostContract(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatalf("读取 registry.json 失败: %v", err)
	}
	var manifest registryManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("registry.json 不是合法 JSON: %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", manifest.SchemaVersion)
	}
	if len(manifest.Plugins) == 0 {
		t.Fatal("registry.json 没有任何插件条目")
	}

	found := false
	for _, plugin := range manifest.Plugins {
		for field, value := range map[string]string{
			"id":          plugin.ID,
			"name":        plugin.Name,
			"description": plugin.Description,
			"author":      plugin.Author,
		} {
			if strings.TrimSpace(value) == "" {
				t.Fatalf("插件 %q 缺少必填字段 %s（宿主会整条拒绝）", plugin.ID, field)
			}
		}
		if !pluginIDPattern.MatchString(plugin.ID) {
			t.Fatalf("插件 id %q 不符合宿主规则", plugin.ID)
		}
		if plugin.Version != "" && !pluginVersionPattern.MatchString(plugin.Version) {
			t.Fatalf("插件 %q 的 version %q 不符合宿主规则", plugin.ID, plugin.Version)
		}
		// install 缺省即 github-release，此时必须有合法的 GitHub 仓库地址。
		if plugin.Install.Type == "" || plugin.Install.Type == "github-release" {
			if !githubRepository.MatchString(plugin.Repository) {
				t.Fatalf("插件 %q 的 repository %q 必须是 https://github.com/{owner}/{repo}", plugin.ID, plugin.Repository)
			}
		}
		if plugin.ID == "qoder2api" {
			found = true
		}
	}
	if !found {
		t.Fatal("registry.json 里必须有 id=qoder2api 的条目（插件 ID 与动态库文件名一致）")
	}
}

// TestRegistryEntryIDMatchesPluginID 保证清单里的 id 与插件自身 ID 一致：
// 不一致时宿主会把库装成别的插件名，plugins.configs 配置与能力声明全部对不上。
func TestRegistryEntryIDMatchesPluginID(t *testing.T) {
	data, err := os.ReadFile("registry.json")
	if err != nil {
		t.Fatalf("读取 registry.json 失败: %v", err)
	}
	var manifest registryManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("registry.json 不是合法 JSON: %v", err)
	}
	for _, plugin := range manifest.Plugins {
		if plugin.ID == pluginID {
			return
		}
	}
	t.Fatalf("registry.json 的条目 id 必须是 %q，实际: %+v", pluginID, manifest.Plugins)
}
