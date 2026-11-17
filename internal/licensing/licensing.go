// Package licensing：离线授权码的签发与解析。
//
// 码的格式是 WPQU1.<base64url(载荷)>.<base64url(签名)>，签的是载荷原始字节。
// 签发与校验放在同一个包里，是为了让「发码的工具」和「验码的面板」绝不可能
// 在字段名或时间格式上跑偏 —— 只改一处的成本远低于两边各改一遍。
//
// 这个包不联网、不读写文件、不碰 HTTP，所以命令行工具和后台服务可以共用。
package licensing

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"wpqu-panel/internal/signing"
)

// Prefix 是码的版本标识，将来改格式时换成 WPQU2。
const Prefix = "WPQU1"

// License 是授权码里携带的载荷。
//
// 字段名短，因为整个载荷要 base64 后塞进一段用户会复制粘贴的文本里。
// 这些 json tag 一旦发出去就是协议，不能改。
type License struct {
	Product string `json:"p"`           // 插件 id
	Expire  string `json:"e"`           // YYYY-MM-DD，空 = 永久
	Seats   int    `json:"s"`           // 可安装机器数，0 = 不限（仅声明，不做技术限制）
	Name    string `json:"n,omitempty"` // 客户标识
	ID      string `json:"i"`           // 授权码唯一 id（对账用）
	Issued  string `json:"t"`           // 签发时间
}

// Parse 用内置的授权公钥校验并解析。
func Parse(code string) (*License, error) {
	return ParseWith(signing.LicensePublicKey, code)
}

// ParseWith 指定公钥解析，供自建发码体系与测试使用。
func ParseWith(pubB64, code string) (*License, error) {
	clean := normalize(code)
	if clean == "" {
		return nil, errors.New("授权码为空")
	}
	parts := strings.Split(clean, ".")
	if len(parts) != 3 {
		return nil, errors.New("授权码格式不对（应为 WPQU1.xxx.yyy）")
	}
	if !strings.EqualFold(parts[0], Prefix) {
		return nil, errors.New("授权码版本标识不对")
	}
	payload, err := decodeB64URL(parts[1])
	if err != nil {
		return nil, errors.New("授权码内容损坏")
	}
	if pubB64 == "" {
		return nil, signing.ErrNoKey
	}
	if err := signing.VerifyWith(pubB64, payload, parts[2]); err != nil {
		if errors.Is(err, signing.ErrNoKey) {
			return nil, err
		}
		return nil, errors.New("授权码校验失败：不是本产品签发的码")
	}

	var lic License
	if err := json.Unmarshal(payload, &lic); err != nil {
		return nil, errors.New("授权码内容无法解析")
	}
	if lic.Product == "" {
		return nil, errors.New("授权码未绑定产品")
	}
	return &lic, nil
}

// Decode 只解码不验签。
//
// 后台排错时需要在「码根本验不过」的情况下也能看到载荷里写了什么，
// 所以必须有一个能绕过签名的入口 —— 它只能用于展示，绝不能用于放行。
func Decode(code string) (*License, error) {
	parts := strings.Split(normalize(code), ".")
	if len(parts) != 3 {
		return nil, errors.New("授权码格式不对")
	}
	payload, err := decodeB64URL(parts[1])
	if err != nil {
		return nil, errors.New("授权码内容损坏")
	}
	var lic License
	if err := json.Unmarshal(payload, &lic); err != nil {
		return nil, errors.New("授权码内容无法解析")
	}
	return &lic, nil
}

// Issue 签发一个码。ID 与 Issued 为空时自动补上。
//
// 返回的 License 就是真实写进码里的内容，调用方应当拿它去记账，
// 不要自己再造一份 —— 两处不一致时，账本会骗人。
func Issue(priv ed25519.PrivateKey, l License) (string, License, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", l, errors.New("私钥不可用")
	}
	l.Product = strings.TrimSpace(l.Product)
	if l.Product == "" {
		return "", l, errors.New("必须指定产品（插件 id）")
	}
	if l.Expire != "" {
		if _, err := time.Parse("2006-01-02", l.Expire); err != nil {
			return "", l, errors.New("到期日格式应为 YYYY-MM-DD")
		}
	}
	if l.ID == "" {
		l.ID = NewID(6)
	}
	if l.Issued == "" {
		l.Issued = time.Now().Format(time.RFC3339)
	}
	body, err := json.Marshal(l)
	if err != nil {
		return "", l, err
	}
	sig := ed25519.Sign(priv, body)
	code := Prefix + "." +
		base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
	return code, l, nil
}

// IssueMany 批量签发，每个码有自己的 ID。
func IssueMany(priv ed25519.PrivateKey, l License, n int) ([]string, []License, error) {
	if n < 1 {
		n = 1
	}
	if n > 500 {
		return nil, nil, errors.New("单次最多 500 个，再多请分批")
	}
	codes := make([]string, 0, n)
	items := make([]License, 0, n)
	for i := 0; i < n; i++ {
		l.ID = ""
		code, item, err := Issue(priv, l)
		if err != nil {
			return codes, items, err
		}
		codes = append(codes, code)
		items = append(items, item)
	}
	return codes, items, nil
}

// Check 判断这个码能不能用于 product（空表示不校验产品）。
func (l *License) Check(product string, now time.Time) error {
	if product != "" && !strings.EqualFold(l.Product, product) {
		return fmt.Errorf("授权码属于 %s，不能用于 %s", l.Product, product)
	}
	if l.Expire != "" {
		exp, err := parseDay(l.Expire)
		if err != nil {
			return errors.New("授权码到期日格式异常")
		}
		// 到当天 23:59:59 为止有效
		if now.After(exp.Add(24*time.Hour - time.Second)) {
			return fmt.Errorf("授权码已于 %s 到期", l.Expire)
		}
	}
	return nil
}

// parseDay 把 YYYY-MM-DD 解析成本地时区的零点。
//
// 必须用 ParseInLocation：time.Parse 给的是 UTC 零点，再和本地 now 比较，
// 在东八区就会凭空多出 8 小时有效期 ——「12-31 到期」实际到 1 月 1 日早上才失效。
func parseDay(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}

// ExpireText 给界面显示用。
func (l *License) ExpireText() string {
	if l.Expire == "" {
		return "永久"
	}
	return l.Expire
}

// Expired 报告是否已过期。
func (l *License) Expired(now time.Time) bool {
	return l.Expire != "" && l.Check("", now) != nil
}

// DaysLeft 返回距到期还有几天；永久授权返回 -1，已过期返回 0。
func (l *License) DaysLeft(now time.Time) int {
	if l.Expire == "" {
		return -1
	}
	exp, err := parseDay(l.Expire)
	if err != nil {
		return 0
	}
	d := int(exp.Add(24*time.Hour-time.Second).Sub(now).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// NewID 生成对账用的短 id，去掉了易混淆的 0/O/1/I。
func NewID(n int) string {
	const alphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"
	if n <= 0 {
		n = 6
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败意味着系统熵源出了问题，此时不该继续发码
		return strings.Repeat("X", n)
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf)
}

// normalize 去掉用户从邮件或聊天窗口里复制时带进来的空白。
func normalize(code string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n', '\u00a0', '\u3000':
			return -1
		}
		return r
	}, strings.TrimSpace(code))
}

func decodeB64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
