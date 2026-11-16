//go:build windows

package updater

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Restart 在 Windows 上的做法：
//
// Windows 不允许覆盖正在运行的 exe，但允许**重命名**它。所以：
//  1. 把新文件复制到同目录 <exe>.new
//  2. 把当前 exe 改名成 <exe>.old（运行中的映像仍可改名，Chrome 等更新器同款手法）
//  3. 把 .new 改名为正式文件名；失败就把 .old 改回来，保证不留下「面板消失」
//  4. 用原来的参数与工作目录启动新进程
//
// 调用方拿到 nil 后应立即请求服务退出。
// 相比「写批处理等进程退出」，这套动作是同步的、可回滚的，不依赖外部脚本能否活着。
func Restart(staged string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法定位当前程序：%w", err)
	}
	exe, _ = filepath.Abs(exe)

	newPath := exe + ".new"
	if err := copyFile(staged, newPath, 0o755); err != nil {
		return fmt.Errorf("准备新版本失败（目录不可写？）：%w", err)
	}

	old := exe + ".old"
	_ = os.Remove(old) // 上次升级的残留，删不掉也不影响本次
	if err := os.Rename(exe, old); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("无法重命名当前程序（权限不足或被其他程序占用）：%w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		// 回滚，绝不能把用户的程序弄丢
		if rerr := os.Rename(old, exe); rerr != nil {
			return fmt.Errorf("替换失败且回滚失败，请手动把 %s 改名为 %s（%v / %v）", old, exe, err, rerr)
		}
		return fmt.Errorf("替换失败，已回滚旧版本：%w", err)
	}

	wd, _ := os.Getwd()
	cmd := exec.Command(exe, restartArgs()...)
	cmd.Dir = wd
	cmd.Env = append(os.Environ(), "WPQU_RESTARTED=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW：有隐藏控制台，别再串 DETACHED_PROCESS
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("新版本已就位，但启动失败：%w。请手动运行 %s", err, exe)
	}
	_ = cmd.Process.Release()
	return nil
}

// CleanupOld 删除上次升级留下的 .old。启动时调用，删不掉（旧进程还没完全退出）也无妨。
func CleanupOld() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	old := exe + ".old"
	if _, err := os.Stat(old); err == nil {
		_ = os.Remove(old)
	}
}

// restartArgs 重启时沿用原命令行参数（去掉 -stop，避免新进程又把自己关掉）。
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
