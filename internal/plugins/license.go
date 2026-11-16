// Package plugins：插件目录、插件包与离线授权码。
package plugins

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"wpqu-panel/internal/signing"
)

// License 是授权码里携带的载荷。
type License struct {
	Product string `json:"p"`           // 插件 id
	Expire  string `json:"e"`           // YYYY-MM-DD，空 = 永久
	Seats   int    `json:"s"`           // 可安装机器数，0 = 不限
	Name    string `json:"n,omitempty"` // 客户标识
	ID      string `json:"i"`           // 授权码唯一 id
	Issued  string `json:"t"`           // 签发时间
}

const licensePrefix = "WPQU1"

// ParseLicense 校验并解析授权码。
//
// 格式：WPQU1.<base64url(payload)>.<base64url(sig)>
// 离线校验的意义：不需要账号体系、不需要联网、不依赖任何服务器存续，
// 你收款后用 wpqusign 签一个码发给客户，面板本地就能判定真伪。
// 代价是没法「吊销」，所以 Seats 和 Expire 要写进载荷。
func ParseLicense(code string) (*License, error) {
	return ParseLicenseWith(signing.LicensePublicKey, code)
}

// ParseLicenseWith 指定公钥解析（自建发码体系 / 测试用）。
func ParseLicenseWith(pubB64, code string) (*License, error) {
	clean := strings.TrimSpace(code)
	// 用户经常从邮件里复制出换行和空格
	clean = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\r', '\n', '\u00a0':
			return -1
		}
		return r
	}, clean)
	if clean == "" {
		return nil, errors.New("授权码为空")
	}

	parts := strings.Split(clean, ".")
	if len(parts) != 3 {
		return nil, errors.New("授权码格式不对（应为 WPQU1.xxx.yyy）")
	}
	if !strings.EqualFold(parts[0], licensePrefix) {
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

// Check 判断这个码能不能用于 product（空表示不校验产品）。
func (l *License) Check(product string, now time.Time) error {
	if product != "" && !strings.EqualFold(l.Product, product) {
		return fmt.Errorf("授权码属于 %s，不能用于 %s", l.Product, product)
	}
	if l.Expire != "" {
		exp, err := time.Parse("2006-01-02", l.Expire)
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

// ExpireText 给界面显示用。
func (l *License) ExpireText() string {
	if l.Expire == "" {
		return "永久"
	}
	return l.Expire
}

func decodeB64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
