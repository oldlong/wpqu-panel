package plugins

import (
	"strings"
	"testing"
)

func TestParseStatus(t *testing.T) {
	out := `###openclaw
INSTALLED	v0.9.3
###golang
MISSING
###python
INSTALLED
1.2.3
`
	got := ParseStatus(out)
	if s := got["openclaw"]; !s.Installed || s.Version != "v0.9.3" {
		t.Errorf("openclaw = %+v", s)
	}
	if s := got["golang"]; s.Installed {
		t.Errorf("golang 应为未安装：%+v", s)
	}
	if s := got["python"]; !s.Installed || s.Version != "1.2.3" {
		t.Errorf("python = %+v", s)
	}
}

func TestParseStatusNoise(t *testing.T) {
	// 远端偶尔会混进 shell 的杂音（motd、sudo 提示），不能因此误判
	out := "Welcome to Ubuntu\n###go\nINSTALLED\tgo version go1.23.4 linux/amd64\n"
	got := ParseStatus(out)
	if s := got["go"]; !s.Installed || s.Version != "go version go1.23.4 linux/amd64" {
		t.Errorf("go = %+v", s)
	}
}

func TestStatusScriptKeepsMultiline(t *testing.T) {
	list := []Manifest{
		{ID: "a", Install: &InstallSpec{Check: "test -f /x\n&& echo ok", VersionCmd: "cat /x/ver"}},
		{ID: "b", Install: nil},
	}
	s := StatusScript(list)
	if !contains(s, "###a") {
		t.Error("缺少 a 的段")
	}
	if contains(s, "###b") {
		t.Error("没有 Install 的插件不该出现")
	}
	// 多行命令必须原样保留，而不是被 && 拼坏
	if !contains(s, "test -f /x\n&& echo ok") {
		t.Error("多行检测命令被改写了")
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func TestValidID(t *testing.T) {
	for _, id := range []string{"openclaw", "go", "py3", "a1-b2"} {
		if !ValidID(id) {
			t.Errorf("ValidID(%q) 应为 true", id)
		}
	}
	for _, id := range []string{"", "a", "../x", "A/b", "中文", "a b"} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) 应为 false", id)
		}
	}
}
