// Package signing：Ed25519 分离签名校验。
//
// 为什么必须验签：面板会把下载回来的可执行文件替换掉自己，也会把插件脚本
// 以 root 身份推到服务器上执行。这两件事一旦被中间人篡改，等于把整台机器
// 交出去。所以清单和安装包一律带 .sig，验不过就不落地。
//
// 签名对象是文件原始字节，不做 JSON 规范化 —— 少一层解析就少一处被绕过的可能。
package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// ReleasePublicKey 用于校验升级清单、插件索引与插件包。
// 对应私钥由发布者离线保管（site/.keys/release.key），不进仓库。
const ReleasePublicKey = "7XaixUDdgiXUdIacWrfdtwhsaB9roPN8+DmQQkcrwcc="

// LicensePublicKey 用于校验用户手里的授权码。
const LicensePublicKey = "us/WIOM04hplimqMMjGA+hXY0NK7RbcYhUwP/oU+Gpw="

var (
	// ErrNoKey 说明本构建没有内置公钥（开发期常见）。
	ErrNoKey = errors.New("此构建未内置验签公钥，无法校验签名")
	// ErrBadSig 签名不匹配。
	ErrBadSig = errors.New("签名校验失败：文件可能被篡改或来自非官方源")
)

func decodeKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "REPLACE_") {
		return nil, ErrNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		// 兼容 base64url
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
		if err != nil {
			return nil, fmt.Errorf("公钥格式异常：%w", err)
		}
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("公钥长度异常：%d", len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyWith 用指定公钥校验 data 的签名（签名参数接受标准/URL 两种 base64）。
func VerifyWith(pubB64 string, data []byte, sig string) error {
	pub, err := decodeKey(pubB64)
	if err != nil {
		return err
	}
	return verify(pub, data, sig)
}

func verify(pub ed25519.PublicKey, data []byte, sig string) error {
	sig = strings.TrimSpace(sig)
	if sig == "" {
		return ErrBadSig
	}
	raw, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(sig, "="))
	}
	if err != nil || len(raw) != ed25519.SignatureSize {
		return ErrBadSig
	}
	if !ed25519.Verify(pub, data, raw) {
		return ErrBadSig
	}
	return nil
}

// VerifyRelease 校验发布产物（升级清单 / 插件索引 / 插件包）。
func VerifyRelease(data []byte, sig string) error {
	return VerifyWith(ReleasePublicKey, data, sig)
}

// VerifyLicense 校验授权码签名。
func VerifyLicense(data []byte, sig string) error {
	return VerifyWith(LicensePublicKey, data, sig)
}

// HasReleaseKey 告诉调用方本构建能不能验签（前端用来提示「开发版不支持升级」）。
func HasReleaseKey() bool {
	_, err := decodeKey(ReleasePublicKey)
	return err == nil
}

// HasLicenseKey 同上。
func HasLicenseKey() bool {
	_, err := decodeKey(LicensePublicKey)
	return err == nil
}
