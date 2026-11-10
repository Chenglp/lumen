//go:build windows

package pipeline

import (
	"errors"
	"syscall"
)

// Windows 错误码。syscall 包没有导出这些常量（它们在 x/sys/windows 里），
// 为一个错误码判断引入一个依赖不值得，这里直接写明数值并标注来源。
//
// 共同点：它们描述的是**设备/链路**状态，不是文件状态。
// 遇到它们必须整体中止扫描——继续走下去每个 stat 都会失败，
// 最后会被当成"所有文件都被删了"，把整卷记录标成消失。
const (
	winErrNotReady       syscall.Errno = 21   // ERROR_NOT_READY，拔盘 / 空驱动器
	winErrBadNetPath     syscall.Errno = 53   // ERROR_BAD_NETPATH
	winErrUnexpNetErr    syscall.Errno = 59   // ERROR_UNEXP_NET_ERR
	winErrNetnameDeleted syscall.Errno = 64   // ERROR_NETNAME_DELETED
	winErrDeviceNotConn  syscall.Errno = 1167 // ERROR_DEVICE_NOT_CONNECTED
	winErrNetUnreachable syscall.Errno = 1231 // ERROR_NETWORK_UNREACHABLE
)

// isVolumeGone 判断一个遍历错误是否意味着「卷整个没了」。
func isVolumeGone(err error) bool {
	codes := []error{
		winErrNotReady,
		winErrDeviceNotConn,
		winErrBadNetPath,
		winErrNetnameDeleted,
		winErrUnexpNetErr,
		winErrNetUnreachable,
	}
	for _, c := range codes {
		if errors.Is(err, c) {
			return true
		}
	}
	return false
}
