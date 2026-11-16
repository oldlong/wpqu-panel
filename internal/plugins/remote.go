package plugins

import (
	"strings"
)

// Status 是某台服务器上某个插件的安装情况。
type Status struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
}

// StatusScript 把 N 个插件的检测命令拼成一次远端执行。
//
// 逐个 SSH 往返在插件多的时候很慢（每个都要新开 channel），
// 合并成一段脚本一次跑完，解析靠 ###<id> 分隔。
func StatusScript(list []Manifest) string {
	var b strings.Builder
	b.WriteString("export LC_ALL=C\n")
	for _, p := range list {
		if p.Install == nil || strings.TrimSpace(p.Install.Check) == "" {
			continue
		}
		b.WriteString("echo '###" + p.ID + "'\n")
		// 检测命令可能是多行，原样放进子 shell —— 换行在 sh 里合法，
		// 不要试图压成一行（用 && 拼会把 if/then 结构弄坏）
		b.WriteString("if (\n" + trimScript(p.Install.Check) + "\n) >/dev/null 2>&1; then\n")
		b.WriteString("  printf 'INSTALLED\\t'\n")
		if strings.TrimSpace(p.Install.VersionCmd) != "" {
			b.WriteString("  (\n" + trimScript(p.Install.VersionCmd) + "\n  ) 2>/dev/null | head -1 || true\n")
		} else {
			b.WriteString("  echo ''\n")
		}
		b.WriteString("else\n  echo MISSING\nfi\n")
	}
	b.WriteString("exit 0\n")
	return b.String()
}

// ParseStatus 解析 StatusScript 的输出。
//
// 期望形态：
//
//	###openclaw
//	INSTALLED	v0.9.3
//	###golang
//	MISSING
func ParseStatus(out string) map[string]Status {
	res := map[string]Status{}
	cur := ""
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "###") {
			cur = strings.TrimSpace(strings.TrimPrefix(trimmed, "###"))
			if cur != "" {
				res[cur] = Status{}
			}
			continue
		}
		if cur == "" || trimmed == "" {
			continue
		}
		if trimmed == "MISSING" {
			res[cur] = Status{}
			cur = ""
			continue
		}
		// 版本行可能粘在 INSTALLED 后面（tab 分隔），也可能单独一行
		if strings.HasPrefix(trimmed, "INSTALLED") {
			rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "INSTALLED"))
			key := cur
			st := res[key]
			st.Installed = true
			if rest != "" {
				st.Version = firstLine(rest)
				cur = "" // 版本只取一行，后面的杂音不再当版本
			}
			res[key] = st
			continue
		}
		st := res[cur]
		st.Installed = true
		st.Version = firstLine(trimmed)
		res[cur] = st
		cur = ""
	}
	return res
}

// trimScript 去掉首尾空行并统一换行，保留内部缩进。
func trimScript(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Trim(s, "\n")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
