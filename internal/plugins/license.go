// Package plugins：插件目录、插件包与离线授权码。
package plugins

import "wpqu-panel/internal/licensing"

// License 是授权码里携带的载荷。
//
// 类型定义在 internal/licensing —— 发码的后台和验码的面板必须共用同一份结构，
// 否则字段名或时间格式一改就会两边不一致，而且只有客户装不上时才会发现。
type License = licensing.License

// ParseLicense 校验并解析授权码（用编译进面板的授权公钥）。
func ParseLicense(code string) (*License, error) {
	return licensing.Parse(code)
}

// ParseLicenseWith 指定公钥解析（自建发码体系 / 测试用）。
func ParseLicenseWith(pubB64, code string) (*License, error) {
	return licensing.ParseWith(pubB64, code)
}
