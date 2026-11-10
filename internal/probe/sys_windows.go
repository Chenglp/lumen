//go:build windows

package probe

import (
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

var (
	k32                      = syscall.NewLazyDLL("kernel32.dll")
	procGetPhysicallyInstMem = k32.NewProc("GetPhysicallyInstalledSystemMemory")
	procGetSystemPowerStatus = k32.NewProc("GetSystemPowerStatus")
	procGetDriveTypeW        = k32.NewProc("GetDriveTypeW")
)

// cpuCount 返回逻辑核数。
//
// 用逻辑核而不是物理核：转码是吞吐型负载，超线程对 x264 是实打实的收益
// （i7-9700K 实测全核比单核快约 5 倍，8 核 8 线程）。拿物理核当上限会白白少开一半。
func cpuCount() int { return runtime.NumCPU() }

// memTotalMB 返回物理内存总量。
//
// 用 GetPhysicallyInstalledSystemMemory 而不是 GlobalMemoryStatusEx：
// 后者报的是"可用地址空间"，在装了核显的机器上会被显存预留吃掉一段，
// 数字看起来像少了一两 G。这个值只用于展示与并发上限判断，误差不影响正确性，
// 但报错了会让人怀疑别处的数字也不准。
func memTotalMB() int {
	var kb uint64
	r, _, _ := procGetPhysicallyInstMem.Call(uintptr(unsafe.Pointer(&kb)))
	if r == 0 {
		return 0
	}
	return int(kb / 1024)
}

type systemPowerStatus struct {
	ACLineStatus        byte
	BatteryFlag         byte
	BatteryLifePercent  byte
	SystemStatusFlag    byte
	BatteryLifeTime     uint32
	BatteryFullLifeTime uint32
}

// onBattery 判断当前是否在用电池供电（C4：电池上不该跑转码）。
//
// ACLineStatus: 0 = 电池, 1 = 交流, 255 = 未知。
// 未知与不可判定一律按"不是电池"处理——把台式机误判成电池会让转码永久停摆，
// 而反过来只是多耗一点电。误判的代价不对称，所以默认值必须偏向可用。
func onBattery() bool {
	var s systemPowerStatus
	r, _, _ := procGetSystemPowerStatus.Call(uintptr(unsafe.Pointer(&s)))
	if r == 0 {
		return false
	}
	return s.ACLineStatus == 0
}

// isNetworkDrive 判断路径是否落在网络盘上。
//
// GetDriveTypeW 返回 4 表示 DRIVE_REMOTE。这一步不能靠"路径里有没有反斜杠"判断：
// 用 `net use Z: \\nas\media` 映射出来的盘，路径看起来和本地盘一模一样。
func isNetworkDrive(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	root := strings.ToUpper(path[:2]) + `\`
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return false
	}
	const driveRemote = 4
	t, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(p)))
	return t == driveRemote
}
