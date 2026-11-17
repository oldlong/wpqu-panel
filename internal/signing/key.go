package signing

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadPrivateKey 读 base64 编码的 Ed25519 私钥文件。
//
// 私钥文件里是 64 字节私钥或 32 字节种子的标准 base64，带不带换行都行 ——
// 用文本编辑器改过的文件经常多一个行尾换行。
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读私钥失败：%w", err)
	}
	return ParsePrivateKey(string(raw))
}

// ParsePrivateKey 解析 base64 私钥文本。
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		// 兼容 base64url，以及被 URL 编码工具处理过的文件
		b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(strings.TrimSpace(s), "="))
		if err != nil {
			return nil, fmt.Errorf("私钥不是合法 base64：%w", err)
		}
	}
	switch len(b) {
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(b), nil
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(b), nil
	}
	return nil, fmt.Errorf("私钥长度异常：%d 字节（应为 32 或 64）", len(b))
}

// Sign 生成 base64 签名。
func Sign(priv ed25519.PrivateKey, data []byte) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
}

// WriteSignature 写 <path>.sig。
//
// 调用方必须传「即将落盘的那段字节」，不要传重新序列化后的 JSON：
// 签名对象与文件内容差一个换行，面板那边就报签名校验失败。
func WriteSignature(path string, data []byte, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("私钥不可用")
	}
	return os.WriteFile(path+".sig", []byte(Sign(priv, data)+"\n"), 0o644)
}

// PublicKeyB64 返回与私钥配对的公钥（base64）。
func PublicKeyB64(priv ed25519.PrivateKey) string {
	if len(priv) != ed25519.PrivateKeySize {
		return ""
	}
	return base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
}

// PublicKeyOf 读私钥文件并返回其公钥（base64）。用于后台启动自检：
// 如果私钥的公钥与编译进面板的常量不一致，发出去的码和包全是废的。
func PublicKeyOf(path string) (string, error) {
	priv, err := LoadPrivateKey(path)
	if err != nil {
		return "", err
	}
	return PublicKeyB64(priv), nil
}
