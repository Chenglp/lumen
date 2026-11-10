//go:build !windows

package domain

import (
	"fmt"
	"path/filepath"
	"strings"
)

// VolumeKeyOf 在非 Windows 平台上暂时不可用。
//
// Linux 要取 blkid UUID 或 /dev/disk/by-uuid 下的名字，macOS 要取
// diskutil 的 VolumeUUID。两者都需要读系统设备信息，排到 S3 前一起做
// （框架 §1.2 ① 的落地项）。在此之前调用方必须显式提供 volume_key，
// 不提供就报错——猜一个等于给自己埋一个永远查不出来的重复库。
func VolumeKeyOf(absPath string) (string, error) {
	return "", fmt.Errorf("domain: %s 上尚未实现卷标识探测，请显式提供 volume_key", platformName())
}

func platformName() string { return "非 Windows 平台" }

// SplitMount 把绝对路径拆成（根, 相对库根）。
func SplitMount(abs string) (mount, rel string, ok bool) {
	clean := filepath.ToSlash(abs)
	if !strings.HasPrefix(clean, "/") {
		return "", "", false
	}
	root := "/"
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
