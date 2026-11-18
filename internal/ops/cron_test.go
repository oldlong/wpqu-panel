package ops

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// 非面板创建的任务在 crontab 里没有标记注释，id 是空串。
// 空串会让前端把 id 当数字用（"" | 0 都能比），所以统一补成 line-<行号>，
// 且类型恒为字符串 —— 这个测试就是盯住这两点。
func TestParseCronStatusFillsLineID(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	out := "PANEL_CRON_AVAIL=1\n" +
		"PANEL_CRON_SVC=running\n" +
		"PANEL_CRON_ENTRY=3|1|0||" + b64("0 3 * * *") + "|" + b64("certbot renew") + "|" + b64("") + "\n" +
		"PANEL_CRON_ENTRY=5|0|1|1763471987|" + b64("*/5 * * * *") + "|" + b64("echo hi") + "|" + b64("自带备注") + "\n" +
		"PANEL_CRON_TOTAL=2\n"

	st := ParseCronStatus(out)
	if len(st.Entries) != 2 {
		t.Fatalf("应解析出 2 条，got %d", len(st.Entries))
	}
	first := st.Entries[0]
	if first.ID != "line-3" {
		t.Fatalf("非面板任务的 id 应是 line-3，got %q", first.ID)
	}
	if first.Managed || first.Line != 3 || first.Command != "certbot renew" {
		t.Fatalf("第一条解析不对：%+v", first)
	}
	second := st.Entries[1]
	if second.ID != "1763471987" || !second.Managed || second.Label != "自带备注" {
		t.Fatalf("面板任务的 id/标记应保留：%+v", second)
	}

	// 序列化后 id 必须是字符串，前端才不用猜类型
	raw, err := json.Marshal(st.Entries[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := back["id"].(string); !ok {
		t.Fatalf("id 序列化后应为字符串，got %T", back["id"])
	}
}

func TestParsePHPStatusCountsSitesPerVersion(t *testing.T) {
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	out := "PANEL_CLI=8.4\n" +
		"PANEL_VER=8.2|1|44\n" +
		"PANEL_VER=8.4|1|45\n" +
		"PANEL_SITE=" + b64("demo.wpqu.com") + "|wordpress|8.4|pool|" + b64("/var/www/demo") + "\n" +
		"PANEL_SITE=" + b64("eeesolar.com") + "|wordpress|8.4|pool|" + b64("/var/www/eeesolar") + "\n" +
		"PANEL_SITE=" + b64("static.wpqu.com") + "|static|-|-|" + b64("/var/www/static") + "\n"

	st := ParsePHPStatus(out)
	if len(st.Versions) != 2 {
		t.Fatalf("应解析出 2 个版本，got %d", len(st.Versions))
	}
	byVer := map[string]PHPVersionInfo{}
	for _, v := range st.Versions {
		byVer[v.Version] = v
	}
	// 站点数挂在版本上，卸载 PHP 时脚本据此拦人；这里恒 0 就等于不拦
	if byVer["8.4"].Sites != 2 || byVer["8.2"].Sites != 0 {
		t.Fatalf("每版站点数不对：8.4=%d 8.2=%d", byVer["8.4"].Sites, byVer["8.2"].Sites)
	}
	if !byVer["8.4"].DefaultCLI {
		t.Fatalf("php 命令指向 8.4，DefaultCLI 应为 true")
	}
	if len(st.Sites) != 3 {
		t.Fatalf("站点列表应有 3 条，got %d", len(st.Sites))
	}
	if st.Sites[0].Domain != "demo.wpqu.com" || st.Sites[0].Root != "/var/www/demo" {
		t.Fatalf("站点解析不对：%+v", st.Sites[0])
	}
}
