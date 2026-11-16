//go:build !windows

package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Restart 在 Linux / macOS 上的做法：
// 把新文件放到同一目录（保证同一文件系统，rename 是原子的），rename 覆盖自身，
// 然后用 syscall.Exec 原地替换进程。正在运行的旧 inode 会被内核回收，
// 不会有「文件被占用」的问题。
//
// syscall.Exec 成功后本进程的代码段已被替换，函数不会返回。
func Restart(staged string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位当前程序：%w", err)
	}
	// 通过软链接启动时（/usr/local/bin/wpqu-panel → /opt/...），要替换真实文件
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	exe, _ = filepath.Abs(exe)

	newPath := exe + ".new"
	if err := copyFile(staged, newPath, 0o755); err != nil {
		return fmt.Errorf("准备新版本失败（目录不可写？）：%w", err)
	}
	if err := os.Chmod(newPath, 0o755); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("设置执行权限失败：%w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("替换失败：%w", err)
	}

	args := append([]string{exe}, restartArgs()...)
	return syscall.Exec(exe, args, os.Environ())
}

// CleanupOld Linux / macOS 上不需要清理，保留空实现让 main 统一调用。
func CleanupOld() {}

// restartArgs 同上：不带 -stop。
func restartArgs() []string {
	out := make([]string, 0, len(os.Args))
	for _, a := range os.Args[1:] {
		if a == "-stop" || a == "--stop" {
			continue
		}
		out = append(out, a)
	}
	return out
}
