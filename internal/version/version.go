// Package version：版本号解析与比较。
//
// 只处理面板自己会遇到的形态：1.9.0、v1.9.0、1.9、1.9.0-beta.2。
// 预发布版（带 - 后缀）小于同号正式版，这样 1.9.0-beta 不会盖掉 1.9.0。
package version

import (
	"strconv"
	"strings"
)

// Parse 把版本号拆成 主/次/修订 + 预发布标记。
// 解析不出来的段落按 0 处理，宁可误判为「不更新」，也不要因为怪版本号反复提示升级。
func Parse(s string) (nums [3]int, pre string) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	s = strings.TrimPrefix(s, "V")
	if i := strings.IndexByte(s, '+'); i >= 0 { // 忽略 build metadata
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre = s[i+1:]
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		n, err := strconv.Atoi(strings.TrimSpace(parts[i]))
		if err != nil || n < 0 {
			n = 0
		}
		nums[i] = n
	}
	return nums, pre
}

// Compare 返回 -1 / 0 / 1。
func Compare(a, b string) int {
	an, ap := Parse(a)
	bn, bp := Parse(b)
	for i := 0; i < 3; i++ {
		if an[i] != bn[i] {
			if an[i] < bn[i] {
				return -1
			}
			return 1
		}
	}
	// 主版本相同时：正式版 > 预发布版
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	if ap == bp {
		return 0
	}
	if ap < bp {
		return -1
	}
	return 1
}

// IsNewer 判断 candidate 是否比 current 新。
func IsNewer(candidate, current string) bool { return Compare(candidate, current) > 0 }

// Valid 粗略判断是不是像样的版本号（至少 主.次）。
func Valid(s string) bool {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if s == "" {
		return false
	}
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return false
	}
	if len(parts) > 3 {
		parts = parts[:3]
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}
