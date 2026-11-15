//go:build windows

package handlers

import "syscall"

// detachedSysProcAttr：Windows 无进程组语义，CREATE_NEW_PROCESS_GROUP 需要
// syscall.SysProcAttr 的 Windows 字段；更新子进程随启动者会话存活即可，
// 暂不做进一步脱离。
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// pidAlive 在 Windows 上不做可靠探活（Signal(0) 不支持）：返回 false 表示
// 「无法确认存活」，pidfile 守卫退化为不拦截。双击应用在 Windows 上会因
// 运行中二进制无法改名而失败，无破坏性。
func pidAlive(pid int) bool {
	return false
}
