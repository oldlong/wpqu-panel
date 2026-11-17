package licensing_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"wpqu-panel/internal/licensing"
	"wpqu-panel/internal/plugins"
)

func newKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, base64.StdEncoding.EncodeToString(pub)
}

func TestIssueAndParse(t *testing.T) {
	priv, pubB64 := newKey(t)
	code, lic, err := licensing.Issue(priv, licensing.License{
		Product: "openclaw", Expire: "2099-12-31", Seats: 2, Name: "某某科技",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(code, "WPQU1.") {
		t.Fatalf("前缀不对：%s", code)
	}
	if lic.ID == "" || lic.Issued == "" {
		t.Fatalf("ID/Issued 应自动补齐：%+v", lic)
	}

	got, err := licensing.ParseWith(pubB64, code)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if got.Product != "openclaw" || got.Seats != 2 || got.Name != "某某科技" || got.ID != lic.ID {
		t.Fatalf("载荷不一致：%+v vs %+v", got, lic)
	}
	if err := got.Check("openclaw", time.Now()); err != nil {
		t.Fatalf("应当有效：%v", err)
	}
	if err := got.Check("golang", time.Now()); err == nil {
		t.Fatal("产品不符应当报错")
	}
}

// 关键回归：后台签发的码必须能被面板自己的解析函数接受。
// 两个包一旦在字段名或时间格式上分叉，客户装插件时才会发现。
func TestIssuedCodeAcceptedByPanel(t *testing.T) {
	priv, pubB64 := newKey(t)
	code, _, err := licensing.Issue(priv, licensing.License{Product: "golang"})
	if err != nil {
		t.Fatal(err)
	}
	lic, err := plugins.ParseLicenseWith(pubB64, code)
	if err != nil {
		t.Fatalf("面板解析失败：%v", err)
	}
	if lic.Product != "golang" {
		t.Fatalf("产品不对：%s", lic.Product)
	}
}

func TestIssueValidation(t *testing.T) {
	priv, _ := newKey(t)
	if _, _, err := licensing.Issue(priv, licensing.License{}); err == nil {
		t.Fatal("缺少产品应当报错")
	}
	if _, _, err := licensing.Issue(priv, licensing.License{Product: "x", Expire: "2099/12/31"}); err == nil {
		t.Fatal("日期格式不对应当报错")
	}
	if _, _, err := licensing.Issue(ed25519.PrivateKey{}, licensing.License{Product: "x"}); err == nil {
		t.Fatal("空私钥应当报错")
	}
}

func TestIssueMany(t *testing.T) {
	priv, pubB64 := newKey(t)
	codes, items, err := licensing.IssueMany(priv, licensing.License{Product: "openclaw", Name: "批量"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 3 || len(items) != 3 {
		t.Fatalf("数量不对：%d/%d", len(codes), len(items))
	}
	seen := map[string]bool{}
	for i, c := range codes {
		lic, err := licensing.ParseWith(pubB64, c)
		if err != nil {
			t.Fatalf("第 %d 个码解析失败：%v", i+1, err)
		}
		if seen[lic.ID] {
			t.Fatalf("ID 重复：%s", lic.ID)
		}
		seen[lic.ID] = true
		if lic.ID != items[i].ID {
			t.Fatalf("返回的载荷与码不一致")
		}
	}
	if _, _, err := licensing.IssueMany(priv, licensing.License{Product: "x"}, 0); err != nil {
		t.Fatalf("count=0 应当按 1 处理：%v", err)
	}
	if _, _, err := licensing.IssueMany(priv, licensing.License{Product: "x"}, 501); err == nil {
		t.Fatal("超过 500 应当拒绝")
	}
}

func TestExpiry(t *testing.T) {
	past := &licensing.License{Product: "x", Expire: "2020-01-01"}
	now := time.Now()
	if !past.Expired(now) {
		t.Fatal("应当已过期")
	}
	if past.DaysLeft(now) != 0 {
		t.Fatal("已过期的剩余天数应为 0")
	}
	// 到期日当天仍然有效
	exp := &licensing.License{Product: "x", Expire: "2026-11-17"}
	if err := exp.Check("", time.Date(2026, 11, 17, 23, 0, 0, 0, time.Local)); err != nil {
		t.Fatalf("当天应有效：%v", err)
	}
	if err := exp.Check("", time.Date(2026, 11, 18, 0, 0, 1, 0, time.Local)); err == nil {
		t.Fatal("次日应失效")
	}
	forever := &licensing.License{Product: "x"}
	if forever.Expired(now) || forever.DaysLeft(now) != -1 || forever.ExpireText() != "永久" {
		t.Fatal("永久授权判断不对")
	}
}

// 只解码不验签：给后台排错用，改一个字节仍能看内容，但解析必须失败。
func TestDecodeIgnoresSignature(t *testing.T) {
	priv, pubB64 := newKey(t)
	code, _, err := licensing.Issue(priv, licensing.License{Product: "openclaw", Name: "被篡改"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(code, ".")
	tampered := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))

	lic, err := licensing.Decode(tampered)
	if err != nil {
		t.Fatalf("解码应当仍可用：%v", err)
	}
	if lic.Name != "被篡改" {
		t.Fatalf("载荷不对：%+v", lic)
	}
	if _, err := licensing.ParseWith(pubB64, tampered); err == nil {
		t.Fatal("验签必须失败")
	}
	if _, err := licensing.Decode("WPQU1.bad"); err == nil {
		t.Fatal("格式不对应当报错")
	}
}

// 从邮件复制出来的码常带空格和换行。
func TestParseToleratesWhitespace(t *testing.T) {
	priv, pubB64 := newKey(t)
	code, _, err := licensing.Issue(priv, licensing.License{Product: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	messy := " " + strings.ReplaceAll(code, ".", ".\n ") + "\r\n"
	if _, err := licensing.ParseWith(pubB64, messy); err != nil {
		t.Fatalf("应当容忍空白：%v", err)
	}
	if _, err := licensing.ParseWith(pubB64, ""); err == nil {
		t.Fatal("空码应当报错")
	}
}
