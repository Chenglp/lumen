//go:build !windows

package pipeline

import (
	"errors"
	"syscall"
)

// isVolumeGone 判断一个遍历错误是否意味着「卷整个没了」。
//
// POSIX 上没有"盘符"这个概念，卷消失表现为挂载点失效，
// errno 通常是 ENODEV / ESTALE（NFS 句柄过期）/ EIO。
// 根目录本身消失的情况由调用方单独判（见 classifyWalkErr）。
func isVolumeGone(err error) bool {
	codes := []error{
		syscall.ENODEV,
		syscall.EIO,
	}
	for _, c := range codes {
		if errors.Is(err, c) {
			return true
		}
	}
	// ESTALE 在部分平台未导出（darwin 有，linux 有），用字符串兜底，
	// 避免为它再拆一个带 build tag 的文件。
	if err != nil && err.Error() != "" {
		msg := err.Error()
		for _, s := range []string{"stale file handle", "no such device", "input/output error"} {
			if containsFold(msg, s) {
				return true
			}
		}
	}
	return false
}

func containsFold(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return false
	}
	// 只做 ASCII 折叠，errno 文本全为 ASCII。
	lower := func(b byte) byte {
		if b >= 'A' && b <= 'Z' {
			return b + 32
		}
		return b
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		ok := true
		for j := 0; j < len(sub); j++ {
			if lower(s[i+j]) != lower(sub[j]) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
