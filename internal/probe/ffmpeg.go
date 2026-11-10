// Package probe 测本机的转码能力，产出 domain.HostProfile。
//
// 为什么必须真测而不能查表：同一句 `-preset veryfast` 在 i9 和 J4125 上差 10 倍；
// `ffmpeg -encoders` 里有 NVENC 不代表这台机器能用（虚拟机、远程桌面会话、
// 消费卡授权不足都会让它一跑就失败）。所以每个结论都要有对应的进程退出码或帧率。
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Bin 是一个已定位的 ffmpeg/ffprobe 对。
type Bin struct {
	FFmpeg     string
	FFprobe    string
	VersionStr string
}

// ErrFFmpegMissing 表示本机没有 ffmpeg。
//
// 这是一个**正常状态**而不是错误状态：没有 ffmpeg 的机器仍然可以直连播放和扫描，
// 只是不能转码。调用方应当落一个 X 档 profile，而不是让服务起不来。
var ErrFFmpegMissing = errors.New("probe: 找不到 ffmpeg")

// Find 定位 ffmpeg 与 ffprobe。
//
// 顺序：环境变量 → PATH → ffmpeg 同目录下的 ffprobe。
// 环境变量优先是为了让用户能指定带 libplacebo 的完整版构建——
// essentials 构建缺 zscale/libplacebo，连 DV 的 tonemap 都做不了（见 R1）。
func Find() (*Bin, error) {
	var candidates []string
	if v := os.Getenv("LUMEN_FFMPEG"); v != "" {
		candidates = append(candidates, v)
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		candidates = append(candidates, p)
	}
	// Windows 上 ffmpeg.exe 有时在 PATH 里但 LookPath 找不到（PATHEXT 被改过）。
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("ffmpeg.exe"); err == nil {
			candidates = append(candidates, p)
		}
	}

	for _, c := range candidates {
		abs, err := filepath.Abs(c)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(abs); err != nil || fi.IsDir() {
			continue
		}
		b := &Bin{FFmpeg: abs}
		// ffprobe 通常与 ffmpeg 同目录。找不到也继续——只是拿不到样本尺寸，
		// 会让缩放退化成软件路径，不影响主流程。
		sibling := filepath.Join(filepath.Dir(abs), exeName("ffprobe"))
		if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
			b.FFprobe = sibling
		}
		return b, nil
	}
	return nil, ErrFFmpegMissing
}

func exeName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// run 执行 ffmpeg/ffprobe 并返回合并后的输出。
//
// stderr 与 stdout 一起收：ffmpeg 把统计行写 stderr，把 -progress 写 stdout，
// 分开收会漏掉其中一半。输出的量级是"每 0.5 秒一行"，几十行，不必担心内存。
func (b *Bin) run(ctx context.Context, timeout time.Duration, bin string, args ...string) ([]byte, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// stdin 必须显式置空：某些构建在 stdin 是终端时会等按键确认，
	// 表现为"探测卡住不动"，而错误信息一个字都没有。
	cmd.Stdin = nil

	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return buf.Bytes(), fmt.Errorf("超时 %s: %s", timeout, bin)
	}
	return buf.Bytes(), err
}

// Encoders 返回 ffmpeg 声称支持的编码器集合。
//
// 注意这只是"声称"：调用方必须再真编一次才算 usable。
func (b *Bin) Encoders(ctx context.Context) (map[string]bool, error) {
	out, err := b.run(ctx, 20*time.Second, b.FFmpeg, "-hide_banner", "-nostdin", "-encoders")
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return parseEncoderList(out), nil
}

// parseEncoderList 解析 `-encoders` 的输出。
//
// 行格式：` V....D h264_nvenc           NVIDIA NVENC H.264 encoder (codec h264)`
// 首列是 6 个字符的能力标志（V/A/S + 5 个点或字母），第二列是名字。
// 不能用"含 h264 就算"来判：那会把 h264 解码器、封装器、滤镜全算进来。
func parseEncoderList(out []byte) map[string]bool {
	set := make(map[string]bool, 64)
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 8 || line[0] != ' ' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		flags := fields[0]
		if len(flags) != 6 {
			continue
		}
		if flags[0] != 'V' && flags[0] != 'A' && flags[0] != 'S' {
			continue
		}
		set[fields[1]] = true
	}
	return set
}

// Features 返回本构建支持的关键滤镜/能力名。
//
// 只关心会影响产品行为的几个，不做通用清单：
//   - libplacebo / zscale：决定能不能正确把 Dolby Vision 转 SDR。
//     essentials 构建两个都没有，DV P5 送进去会静默变成绿片（R1 已实测）。
//   - tonemap / tonemap_opencl：HDR10→SDR 的常规路径。
func (b *Bin) Features(ctx context.Context) ([]string, error) {
	out, err := b.run(ctx, 20*time.Second, b.FFmpeg, "-hide_banner", "-nostdin", "-filters")
	var feats []string
	if err == nil || len(out) > 0 {
		for _, f := range []string{"libplacebo", "zscale", "tonemap", "tonemap_opencl", "scale_cuda", "scale_qsv"} {
			if hasFilter(out, f) {
				feats = append(feats, f)
			}
		}
	}
	return feats, err
}

func hasFilter(out []byte, name string) bool {
	// 滤镜行的名字在第三列：` ... tonemap            V->V       Tone-map ...`
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == name {
			return true
		}
	}
	return false
}

// Version 取 ffmpeg 版本串的第一行，并缓存到 VersionStr。
func (b *Bin) Version(ctx context.Context) string {
	if b.VersionStr != "" {
		return b.VersionStr
	}
	out, _ := b.run(ctx, 20*time.Second, b.FFmpeg, "-hide_banner", "-nostdin", "-version")
	line := string(out)
	if i := strings.IndexByte(line, '\n'); i > 0 {
		line = line[:i]
	}
	b.VersionStr = strings.TrimSpace(line)
	return b.VersionStr
}
