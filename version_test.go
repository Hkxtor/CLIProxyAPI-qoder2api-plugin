package main

import "testing"

// TestEffectivePluginVersion 锁死版本号来源：
//   - CI 用 `-ldflags -X main.pluginVersion=<tag>` 注入发行版号时按注入值上报；
//   - 注入为空（构建脚本变量写错、tag 名为空）时回落到默认版本，
//     避免管理端与注册信息里出现空版本号；
//   - 注入值带首尾空白时也要修剪。
func TestEffectivePluginVersion(t *testing.T) {
	original := pluginVersion
	defer func() { pluginVersion = original }()

	cases := []struct {
		injected string
		want     string
	}{
		{"v1.2.3", "v1.2.3"},
		{"  v0.9.0  ", "v0.9.0"},
		{"", defaultPluginVersion},
		{"   ", defaultPluginVersion},
	}
	for _, testCase := range cases {
		pluginVersion = testCase.injected
		if got := effectivePluginVersion(); got != testCase.want {
			t.Fatalf("pluginVersion=%q → %q, want %q", testCase.injected, got, testCase.want)
		}
	}
	if defaultPluginVersion == "" {
		t.Fatal("defaultPluginVersion must not be empty")
	}
}
