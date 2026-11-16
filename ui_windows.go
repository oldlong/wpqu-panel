//go:build windows

package main

import (
	"os/exec"
	"syscall"
	"unsafe"
)

// openBrowser 用 rundll32 打开默认浏览器。
// 不用 `cmd /c start`：后者会闪一个黑窗口，正是要避免的东西。
func openBrowser(url string) error {
	return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
}

// hasConsole 判断当前进程是否挂着控制台（双击启动时为 false）。
func hasConsole() bool {
	proc := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow")
	h, _, _ := proc.Call()
	return h != 0
}

// messageBox 在无控制台时提示用户（启动失败 / 面板已停止等）。
func messageBox(title, text string) {
	proc := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	t, err1 := syscall.UTF16PtrFromString(title)
	b, err2 := syscall.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	const mbIconInfo = 0x00000040
	_, _, _ = proc.Call(0,
		uintptr(unsafe.Pointer(b)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbIconInfo),
	)
}
