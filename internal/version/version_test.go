package version

import "testing"

func TestCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.9.0", "1.8.0", 1},
		{"1.8.0", "1.9.0", -1},
		{"1.8.0", "1.8.0", 0},
		{"v1.10.0", "1.9.9", 1}, // 字符串比较会判错，必须按数字
		{"1.8", "1.8.0", 0},
		{"1.9.0-beta.1", "1.9.0", -1},
		{"1.9.1", "1.9.0-beta.1", 1},
		{"2.0.0", "1.99.99", 1},
		{"", "1.0.0", -1},
		{"1.8.0+abc", "1.8.0", 0},
	}
	for _, c := range cases {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%q,%q)=%d want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestValid(t *testing.T) {
	for _, s := range []string{"1.9", "1.9.0", "v1.9.0", "10.0.1-beta"} {
		if !Valid(s) {
			t.Errorf("Valid(%q) 应为 true", s)
		}
	}
	for _, s := range []string{"", "1", "abc", "1.x", "v"} {
		if Valid(s) {
			t.Errorf("Valid(%q) 应为 false", s)
		}
	}
}
