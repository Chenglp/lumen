//go:build !windows

package probe

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// cpuCount 返回逻辑核数。
func cpuCount() int { return runtime.NumCPU() }

// memTotalMB 从 /proc/meminfo 读物理内存。
//
// 读不到就返回 0，不引 syscall.Sysinfo：那个调用在不同 unix 上的结构体布局
// 不一样，为一行展示用的数字引平台分支不值得。0 表示"不知道"，下游不会因此判错档。
func memTotalMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			return 0
		}
		return int(kb / 1024)
	}
	return 0
}

// onBattery 通过 /sys/class/power_supply 判断是否在用电池。
//
// 逻辑：找到 type= Battery 的设备且它的 status 是 Discharging，就算在用电池。
// 只看 AC 设备的 online 会漏掉"插着电源但没在充电"的机器。
// 读不到任何电源信息（台式机、容器）就返回 false。
func onBattery() bool {
	entries, err := filepath.Glob("/sys/class/power_supply/*")
	if err != nil {
		return false
	}
	for _, dir := range entries {
		t, err := os.ReadFile(filepath.Join(dir, "type"))
		if err != nil || strings.TrimSpace(string(t)) != "Battery" {
			continue
		}
		st, err := os.ReadFile(filepath.Join(dir, "status"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(st)) == "Discharging" {
			return true
		}
	}
	return false
}

// isNetworkDrive 在非 Windows 上恒为 false。
//
// Linux/macOS 的网络挂载没有统一的 Windows 盘符概念，靠 st_dev 或 /proc/mounts
// 判断会把这件事变成一堆平台分支。目前探测只用它决定"要不要做磁盘写入测试"，
// 判错最多是多做一次写入或跳过写入，不影响档位——所以先不猜。
func isNetworkDrive(string) bool { return false }
