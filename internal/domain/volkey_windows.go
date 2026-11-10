//go:build windows

package domain

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetVolumeInfoW = kernel32.NewProc("GetVolumeInformationW")
)

// VolumeKeyOf 返回 absPath 所在卷的稳定标识。
//
// Windows 上取的是 NTFS 卷序列号（8 位大写十六进制），与 `vol C:` 输出一致。
//
// 为什么盘符不能当身份：盘符是"这次开机分给这块盘的名字"。拔掉 D: 再插可能
// 变成 E:，用户还会把两块盘交替插同一个口。此时 volume_key 不变、rel_path 不变，
// 库里那条记录才认得出是同一部片子（框架 §1.2 ①）。
//
// 已知限制：卷被重新格式化后序列号会变，库需要重建——这是正确的行为，
// 因为文件也确实全没了。
func VolumeKeyOf(absPath string) (string, error) {
	root := volumeRoot(absPath)
	if root == "" {
		// 不猜。相对路径算卷身份这件事本身就是错的，静默补全只会埋坑。
		return "", errors.New("domain: 需要绝对路径才能确定所在卷")
	}

	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return "", fmt.Errorf("domain: 卷根 %q 转换失败: %w", root, err)
	}

	var (
		serial    uint32
		maxComp   uint32
		flags     uint32
		volName   [syscall.MAX_PATH + 1]uint16
		fsName    [syscall.MAX_PATH + 1]uint16
		oneBuffer = len(volName)
	)
	r1, _, callErr := procGetVolumeInfoW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&volName[0])), uintptr(oneBuffer),
		uintptr(unsafe.Pointer(&serial)),
		uintptr(unsafe.Pointer(&maxComp)),
		uintptr(unsafe.Pointer(&flags)),
		uintptr(unsafe.Pointer(&fsName[0])), uintptr(len(fsName)),
	)
	if r1 == 0 {
		// 盘已拔出、未格式化、或权限不足都会走到这里。
		// 调用方应当把它当作"卷不可用"，而不是"这个库不存在"。
		return "", fmt.Errorf("domain: 读取卷信息 %s: %w", root, callErr)
	}
	return fmt.Sprintf("%08X", serial), nil
}

// volumeRoot 从绝对路径截出卷根：`F:\a\b` → `F:\`，UNC → `\\host\share\`。
func volumeRoot(abs string) string {
	if len(abs) >= 2 && abs[1] == ':' {
		return strings.ToUpper(abs[:2]) + `\`
	}
	if strings.HasPrefix(abs, `\\`) {
		// UNC 的卷是共享名，不是服务器名：\\host\share\a\b → \\host\share\。
		parts := strings.SplitN(abs[2:], `\`, 3)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return ""
		}
		return `\\` + parts[0] + `\` + parts[1] + `\`
	}
	return ""
}

// SplitMount 把绝对路径拆成（挂载点, 卷相对库根）。
//
// `F:\movies` → (`F:\`, `movies`)。库根必须相对卷，因为库的身份是
// (volume_id, root_rel)——两个卷上都可以有 movies 目录。
func SplitMount(abs string) (mount, rel string, ok bool) {
	root := volumeRoot(abs)
	if root == "" {
		return "", "", false
	}
	clean := strings.ReplaceAll(abs, `\`, `/`)
	rootSlash := strings.ReplaceAll(root, `\`, `/`) // 形如 `F:/` 或 `//host/share/`
	lower := strings.ToLower(clean)
	lroot := strings.ToLower(rootSlash)

	// 盘符与共享名不区分大小写（f: 和 F: 是同一块盘），
	// 但余下的目录名要保留原始大小写——显示和 rel_path 都靠它。
	switch {
	case strings.HasPrefix(lower, lroot):
		clean = clean[len(rootSlash):]
	case strings.HasPrefix(lower+"/", lroot):
		// 传入的就是卷根本身（`\\nas\share` 带不带尾斜杠两种写法）。
		clean = ""
	default:
		return "", "", false
	}

	rest := strings.Trim(clean, "/")
	if rest == "" {
		return root, ".", true
	}
	r, valid := CleanRel(rest)
	if !valid {
		return "", "", false
	}
	return root, r, true
}
