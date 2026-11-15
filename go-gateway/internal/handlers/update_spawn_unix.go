//go:build !windows

package handlers

import (
	"errors"
	"syscall"
)

// detachedSysProcAttr 让更新子进程脱离当前进程组：网关被更新重启时
// （kickstart/try-restart 杀掉本进程），子进程照常完成健康轮询与回滚。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// pidAlive 用信号 0 探活；EPERM 同样说明进程存在（权限不足但活着）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
