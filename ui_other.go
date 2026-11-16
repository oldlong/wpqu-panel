//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

// openBrowser 在 macOS / Linux 上调系统默认浏览器。
func openBrowser(url string) error {
	for _, bin := range []string{"open", "xdg-open"} {
		if _, err := exec.LookPath(bin); err == nil {
			return exec.Command(bin, url).Start()
		}
	}
	return errors.New("找不到 open / xdg-open 命令")
}

// 非 Windows 平台总是有终端，不需要弹窗。
func hasConsole() bool { return true }

func messageBox(title, text string) {}
