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

func popupError(title, text string) { messageBox(title, text, true) }
func popupInfo(title, text string)  { messageBox(title, text, false) }

// messageBox 弹一个系统消息框。双击启动时控制台窗口会随进程退出而关闭，
// 报错和新口令只有弹窗才留得住。（消息框里的文字可以 Ctrl+C 复制。）
func messageBox(title, text string, isErr bool) {
	proc := syscall.NewLazyDLL("user32.dll").NewProc("MessageBoxW")
	t, err1 := syscall.UTF16PtrFromString(title)
	b, err2 := syscall.UTF16PtrFromString(text)
	if err1 != nil || err2 != nil {
		return
	}
	const (
		mbOK        = 0x00000000
		mbIconError = 0x00000010
		mbIconInfo  = 0x00000040
		mbTopmost   = 0x00040000
	)
	flags := uintptr(mbIconInfo | mbOK | mbTopmost)
	if isErr {
		flags = uintptr(mbIconError | mbOK | mbTopmost)
	}
	_, _, _ = proc.Call(0,
		uintptr(unsafe.Pointer(b)),
		uintptr(unsafe.Pointer(t)),
		flags,
	)
}
