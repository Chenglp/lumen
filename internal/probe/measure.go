package probe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 测量窗口。
//
// 这个数不决定准确度（准确度靠基线扣除，见 measureWithBaseline），只决定噪声：
// 太短则帧数少、抖动大；太长则首次启动等太久。8 秒在 60fps 源上是 480 帧，
// 在 1.1× 的软编链路上要跑 7 秒，两边都在"够稳定"和"不至于让人等"之间。
const measureSeconds = 8

// fastMeasureSeconds 是 --probe-fast 用的窗口，更短更不准但快。
const fastMeasureSeconds = 3

// baselineSeconds 是基线运行的时长：只够打开文件、seek、初始化编码器，
// 不含实质工作量。它的墙钟时间就是要从正式测量里扣掉的那一段。
const baselineSeconds = 1

// verifyFrames 只验证"能不能开"，24 帧足够。
const verifyFrames = 24

// sampleInfo 是 ffprobe 对样本的描述。
type sampleInfo struct {
	Width, Height int
	FPS           float64
	DurationS     float64
	PixelFormat   string
	ColorTransfer string
	HDR           string // sdr | hdr10 | hlg | dv5 | dv7 | dv8 | dv_other
}

// probeSample 用 ffprobe 读样本的关键参数。
func (b *Bin) probeSample(ctx context.Context, path string) (sampleInfo, error) {
	var si sampleInfo
	if b.FFprobe == "" {
		return si, fmt.Errorf("probe: 缺少 ffprobe")
	}
	args := []string{
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries",
		"stream=width,height,pix_fmt,color_transfer,r_frame_rate:stream_side_data=side_data_type,dv_profile:format=duration",
		"-of", "json", path,
	}
	out, err := b.run(ctx, 30*time.Second, b.FFprobe, args...)
	if err != nil && len(out) == 0 {
		return si, err
	}

	var raw struct {
		Streams []struct {
			Width         int    `json:"width"`
			Height        int    `json:"height"`
			PixelFormat   string `json:"pix_fmt"`
			ColorTransfer string `json:"color_transfer"`
			FrameRate     string `json:"r_frame_rate"`
			SideDataList  []struct {
				Type      string `json:"side_data_type"`
				DVProfile any    `json:"dv_profile"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return si, fmt.Errorf("probe: 解析 ffprobe 输出: %w", err)
	}
	if len(raw.Streams) == 0 {
		return si, fmt.Errorf("probe: 样本里没有视频流")
	}
	s := raw.Streams[0]
	si.Width, si.Height = s.Width, s.Height
	si.PixelFormat = s.PixelFormat
	si.ColorTransfer = s.ColorTransfer
	si.FPS = parseRational(s.FrameRate)
	si.DurationS, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	si.HDR = classifyHDR(s.ColorTransfer, s.SideDataList)
	return si, nil
}

func parseRational(s string) float64 {
	if i := strings.IndexByte(s, '/'); i > 0 {
		num, err1 := strconv.ParseFloat(s[:i], 64)
		den, err2 := strconv.ParseFloat(s[i+1:], 64)
		if err1 == nil && err2 == nil && den > 0 {
			return num / den
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

// classifyHDR 判定 HDR 类型。
//
// Dolby Vision 必须分 profile：P5 没有 HDR10 基础层，非 DV 客户端会把它当
// 普通 YCbCr 解出来，整片偏绿而且**不报错**；P7/P8 带基础层，那些客户端
// 当 HDR10 播颜色是对的。这两类不能用同一个枚举合并（契约 v1.9 规则 0b）。
func classifyHDR(transfer string, side []struct {
	Type      string `json:"side_data_type"`
	DVProfile any    `json:"dv_profile"`
}) string {
	for _, sd := range side {
		if !strings.Contains(strings.ToUpper(sd.Type), "DOVI") &&
			!strings.Contains(strings.ToLower(sd.Type), "dolby") {
			continue
		}
		switch v := sd.DVProfile.(type) {
		case float64:
			return dvName(int(v))
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return dvName(n)
			}
		}
		return "dv_other"
	}
	switch transfer {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	}
	return "sdr"
}

func dvName(p int) string {
	switch p {
	case 5:
		return "dv5"
	case 7:
		return "dv7"
	case 8:
		return "dv8"
	}
	return "dv_other"
}

// IsDolbyVisionProfile5 判断是否是不兼容 HDR10 的那一类。
func (s sampleInfo) IsDolbyVisionProfile5() bool { return s.HDR == "dv5" }

// stats 是一次测量的结果。
type stats struct {
	FPS      float64 // 采用的帧率
	Frames   int     // 处理的总帧数
	Wall     float64 // 正式运行的墙钟耗时（秒）
	Baseline float64 // 基线运行耗时（秒），即启动 + seek + 初始化
	Method   string  // baseline | wall，记录这个 fps 是怎么算出来的
}

var (
	reProgressFrame = regexp.MustCompile(`^frame=\s*([0-9]+)\s*$`)
	reStderrFrame   = regexp.MustCompile(`frame=\s*([0-9]+)`)
	reStderrFPS     = regexp.MustCompile(`fps=\s*([0-9]+(?:\.[0-9]+)?)`)
)

// ParseFrames 从 ffmpeg 的 stderr 里取最后一次上报的帧数。
func ParseFrames(out []byte) int {
	all := reStderrFrame.FindAllSubmatch(out, -1)
	for i := len(all) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(string(all[i][1])); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// ParseFPS 取 ffmpeg 自报的帧率，只用于日志对照。
//
// 不能当测量结果用：实测发现短片段的统计行全是 `fps=0.0`，
// 而且这个数算的是"距上次上报的增量"，反映瞬时值而不是稳态值。
func ParseFPS(out []byte) float64 {
	all := reStderrFPS.FindAllSubmatch(out, -1)
	for i := len(all) - 1; i >= 0; i-- {
		if v, err := strconv.ParseFloat(string(all[i][1]), 64); err == nil && v > 0 {
			return v
		}
	}
	return 0
}

// videoRun 是一次 ffmpeg 运行的原始观测。
type videoRun struct {
	Wall   float64
	Frames int
}

// runOnce 跑一次并统计帧数与墙钟。
//
// 帧数从 `-progress pipe:1` 读，不从 stderr 读：加上 `-loglevel error`
// 之后 stderr 的统计行会被整段抑制（实测确认），而 -progress 与日志级别无关。
func (b *Bin) runOnce(ctx context.Context, timeout time.Duration, args []string) (videoRun, error) {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := []string{"-hide_banner", "-nostdin", "-nostats", "-progress", "pipe:1"}
	full = append(full, args...)

	cmd := exec.CommandContext(cctx, b.FFmpeg, full...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return videoRun{}, fmt.Errorf("probe: 建立进度管道: %w", err)
	}
	var errBuf bytes.Buffer
	var mu sync.Mutex
	cmd.Stderr = &lockedWriter{buf: &errBuf, mu: &mu}
	cmd.Stdin = nil

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return videoRun{}, fmt.Errorf("probe: 启动 ffmpeg: %w", err)
	}

	frames := 0
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if m := reProgressFrame.FindStringSubmatch(sc.Text()); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				frames = n
			}
		}
	}
	// stdout 必须读完再 Wait，否则子进程可能因管道写满而阻塞。
	io.Copy(io.Discard, stdout)

	waitErr := cmd.Wait()
	wall := time.Since(start).Seconds()

	if cctx.Err() == context.DeadlineExceeded {
		return videoRun{}, fmt.Errorf("probe: 测量超时 %s", timeout)
	}
	mu.Lock()
	errOut := append([]byte(nil), errBuf.Bytes()...)
	mu.Unlock()

	if frames == 0 {
		frames = ParseFrames(errOut)
	}
	if frames == 0 {
		if waitErr != nil {
			return videoRun{}, fmt.Errorf("probe: ffmpeg 失败: %s", lastLine(errOut))
		}
		return videoRun{}, fmt.Errorf("probe: 一帧都没处理")
	}
	if waitErr != nil {
		// 帧数有了但退出码非 0：通常是编码器在收尾时报错。这时测出来的
		// 吞吐不可信，直接判失败，让上层换下一个方案。
		return videoRun{}, fmt.Errorf("probe: ffmpeg 异常退出: %s", lastLine(errOut))
	}
	return videoRun{Wall: wall, Frames: frames}, nil
}

// measureWithBaseline 用"正式运行减基线"的方式算稳态帧率。
//
// 为什么必须减基线：打开一个 33 GB 的 mkv 并 seek 到 120 秒处要 ~0.85 秒，
// 这段时间一帧都没处理。不减的话，8 秒窗口里 10% 是纯开销，而且窗口越短越糟。
//
// 为什么不用 ffmpeg 自报的 fps：实测短片段里它只报 `fps=0.0`，
// 而且它算的是"距上次上报的增量"，是瞬时值不是稳态值。
//
// 为什么不用"进度事件的时间差"：管道会攒数据，几行进度可能一起被读到，
// 时间戳挤在一起会把帧率算高一倍。这个方法看起来能用，实测不稳定。
//
// 基线放在正式运行**之后**测，这一点很重要：第一次运行 ffmpeg 要多花 ~90ms
// （加载二进制、杀软扫描）。先跑正式再跑基线，基线偏小 → 扣得少 → 帧率偏低。
// 误差落在保守方向，可以接受；反过来会把机器估强。
func (b *Bin) measureWithBaseline(ctx context.Context, secs int,
	build func(int) []string) (stats, error) {

	main, err := b.runOnce(ctx, 0, build(secs))
	if err != nil {
		return stats{}, err
	}
	base, baseErr := b.runOnce(ctx, 0, build(baselineSeconds))

	s := stats{Frames: main.Frames, Wall: main.Wall}
	if baseErr == nil {
		s.Baseline = base.Wall
	}

	work := main.Wall - s.Baseline
	switch {
	case baseErr == nil && work > 0.05:
		s.FPS = float64(main.Frames) / work
		s.Method = "baseline"
	case main.Wall > 0:
		// 基线没测到：退化成总帧数/墙钟。这个数一定偏低，是保守方向。
		s.FPS = float64(main.Frames) / main.Wall
		s.Method = "wall"
	}
	if s.FPS <= 0 {
		return s, fmt.Errorf("probe: 没有测到帧率")
	}
	return s, nil
}

// lockedWriter 让 stderr 的并发写安全。
type lockedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// lastLine 取输出的最后一行非空内容，把 ffmpeg 的报错压成一句话。
func lastLine(out []byte) string {
	text := strings.ReplaceAll(string(out), "\r", "\n")
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		s := strings.TrimSpace(lines[i])
		if s != "" && !strings.HasPrefix(s, "frame=") {
			return s
		}
	}
	return ""
}

// plan 是一次测量的执行方案：硬件解码参数 + 对应的缩放滤镜。
//
// 硬件解码与硬件编码跑在不同引擎上、是**重叠**的，所以有硬解时全链吞吐
// 会显著高于"软件解码 + 硬件编码"。R1 实测：仅 NVDEC 137 fps，
// NVDEC + NVENC 1080p 254 fps——全链比单解码还快，这就是重叠的证据。
// 也正因为如此，**不能**用 1/(1/解码 + 1/编码) 估算全链：那条式子只在串行时成立。
type plan struct {
	name  string
	accel []string // 放在 -i 之前
	scale string   // 缩放滤镜模板，%d/%d 替换成宽高
}

// plansFor 返回该编码器可尝试的方案，按优先级排列。
//
// 只对能在这个构建里验证的厂商做硬件解码方案。未列出的走软件解码：
// 慢一些但一定对，而且我们**实测的就是这条链路**，所以报出来的数仍然是真的。
// 宁可少报，也不要报一个用不了的漂亮数字。
func plansFor(enc string) []plan {
	sw := plan{name: "software", scale: "scale=%d:%d"}
	switch {
	case strings.HasSuffix(enc, "_nvenc"):
		return []plan{
			{name: "cuda", accel: []string{"-hwaccel", "cuda", "-hwaccel_output_format", "cuda"},
				scale: "scale_cuda=%d:%d:format=nv12"},
			sw,
		}
	case strings.HasSuffix(enc, "_videotoolbox"):
		return []plan{
			{name: "videotoolbox", accel: []string{"-hwaccel", "videotoolbox"}, scale: "scale=%d:%d"},
			sw,
		}
	case strings.HasSuffix(enc, "_qsv"):
		return []plan{
			{name: "qsv", accel: []string{"-hwaccel", "qsv", "-hwaccel_output_format", "qsv"},
				scale: "scale_qsv=w=%d:h=%d"},
			sw,
		}
	}
	return []plan{sw}
}

// encoderArgs 返回该编码器的预设参数。
//
// 刻意用"快"而不是"好"：探测要回答的是"这台机器能不能实时"。用 slow 预设
// 测出来的是最坏情况，会把一台本来够用的机器判成不够用。真实转码时的预设
// 由播放决策另挑，与这里无关。
func encoderArgs(enc string) []string {
	switch {
	case strings.HasSuffix(enc, "_nvenc"):
		return []string{"-preset", "p4"}
	case strings.HasSuffix(enc, "_qsv"):
		return []string{"-preset", "veryfast"}
	case strings.HasSuffix(enc, "_amf"):
		return []string{"-quality", "speed"}
	case strings.HasSuffix(enc, "_videotoolbox"):
		return []string{"-q:v", "65"}
	case enc == "libx264":
		return []string{"-preset", "veryfast", "-crf", "23"}
	case enc == "libx265":
		return []string{"-preset", "veryfast", "-crf", "26"}
	}
	return nil
}

// hwCandidates 是硬件编码器的尝试顺序。
//
// h264 优先于 hevc：兼容性更广（老电视、浏览器 MSE）。选错编码器不会报错，
// 只会变成"黑屏有声音"。
var hwCandidates = []string{
	"h264_nvenc", "h264_videotoolbox", "h264_qsv", "h264_amf", "h264_vaapi",
	"hevc_nvenc", "hevc_videotoolbox", "hevc_qsv", "hevc_amf", "hevc_vaapi",
}

// softCandidates 是软编尝试顺序。
var softCandidates = []string{"libx264", "libx265"}

// verifyEncoder 真编一小段，判断这个编码器到底能不能用。
//
// 为什么非编不可：`ffmpeg -encoders` 里出现 NVENC 只说明这个构建编译进了
// 该编码器，与这台机器的驱动/显卡/会话类型无关。虚拟机里没有 GPU 透传、
// 远程桌面会话拿不到编码会话、消费卡授权耗尽，都会让"列表里有"变成
// "开编码器就失败"。只有退出码能回答这个问题。
func (b *Bin) verifyEncoder(ctx context.Context, enc string) (bool, string) {
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=24",
		"-frames:v", strconv.Itoa(verifyFrames),
		"-c:v", enc,
	}
	args = append(args, encoderArgs(enc)...)
	args = append(args, "-f", "null", "-")

	out, err := b.run(ctx, 30*time.Second, b.FFmpeg, args...)
	if err != nil {
		why := lastLine(out)
		if why == "" {
			why = err.Error()
		}
		return false, why
	}
	return true, ""
}

// MeasureChain 测全链吞吐：源 → 目标分辨率 → 编码。
//
// 支持多个取样点，取其中**最慢**的那个。原因是实测出来的：同一台机器、
// 同一条命令，120 秒处取 8 秒测出 81 fps，取 30 秒测出 72 fps——
// 差 13%，纯粹因为两个片段的内容不同（动作戏比对话戏难压）。
// 只测一个点就会随场景漂移，而"能不能实时"这个判断不能看运气。
// 取最小值是保守方向：多估一点等待时间，比承诺了却卡住好。
//
// 硬件方案失败会自动退到软件并重测——这比"先假设硬件能用、失败再整体重来"
// 少一次完整测量，也保证报出来的数对应真正会走的那条链路。
func (b *Bin) MeasureChain(ctx context.Context, sample string, seeks []int, secs int,
	enc string, dstW, dstH int) (fps float64, planName string, err error) {

	if len(seeks) == 0 {
		seeks = []int{0}
	}
	var lastErr error
	for _, p := range plansFor(enc) {
		best := 0.0
		ok := 0
		for _, seek := range seeks {
			build := func(t int) []string {
				args := []string{"-loglevel", "error"}
				if seek > 0 {
					args = append(args, "-ss", strconv.Itoa(seek))
				}
				args = append(args, "-t", strconv.Itoa(t))
				args = append(args, p.accel...)
				args = append(args, "-i", sample, "-map", "0:v:0", "-an", "-sn",
					"-vf", fmt.Sprintf(p.scale, dstW, dstH), "-c:v", enc)
				args = append(args, encoderArgs(enc)...)
				args = append(args, "-b:v", "8M", "-f", "null", "-")
				return args
			}
			s, e := b.measureWithBaseline(ctx, secs, build)
			if e != nil {
				lastErr = fmt.Errorf("%s 方案 @%ds: %w", p.name, seek, e)
				continue
			}
			ok++
			if best == 0 || s.FPS < best {
				best = s.FPS
			}
		}
		if ok > 0 {
			return best, p.name, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("probe: 没有可用方案")
	}
	return 0, "", lastErr
}

// MeasureDecode 只解码不编码，用于区分"解码慢"还是"编码慢"。
func (b *Bin) MeasureDecode(ctx context.Context, sample string, seek, secs int) (float64, error) {
	build := func(t int) []string {
		args := []string{"-loglevel", "error"}
		if seek > 0 {
			args = append(args, "-ss", strconv.Itoa(seek))
		}
		args = append(args, "-t", strconv.Itoa(t),
			"-i", sample, "-map", "0:v:0", "-an", "-sn", "-f", "null", "-")
		return args
	}
	s, err := b.measureWithBaseline(ctx, secs, build)
	if err != nil {
		return 0, fmt.Errorf("probe: 解码测量失败: %w", err)
	}
	return s.FPS, nil
}

// MeasureTonemap 测 Dolby Vision → SDR 的全链吞吐。
//
// 必须用软件解码：libplacebo 在 CPU 侧工作，喂不进 cuda 帧。所以这一条
// 享受不到硬解加速，实测就是这么慢（R1：4K60 DV 只有 1.18×）。
func (b *Bin) MeasureTonemap(ctx context.Context, sample string, seek, secs,
	dstW, dstH int, enc string) (float64, error) {

	vf := fmt.Sprintf("libplacebo=w=%d:h=%d:tonemapping=bt.2390:apply_dolbyvision=true:format=yuv420p", dstW, dstH)
	build := func(t int) []string {
		args := []string{"-loglevel", "error"}
		if seek > 0 {
			args = append(args, "-ss", strconv.Itoa(seek))
		}
		args = append(args, "-t", strconv.Itoa(t),
			"-i", sample, "-map", "0:v:0", "-an", "-sn", "-vf", vf, "-c:v", enc)
		args = append(args, encoderArgs(enc)...)
		args = append(args, "-b:v", "8M", "-f", "null", "-")
		return args
	}
	s, err := b.measureWithBaseline(ctx, secs, build)
	if err != nil {
		return 0, fmt.Errorf("probe: DV tonemap 失败: %w", err)
	}
	return s.FPS, nil
}

// MeasureSyntheticChain 在没有真实片源时测"缩放 + 编码"这一段。
//
// 它不包含解码，所以结果必然偏乐观——调用方必须按 estimateFromSynthetic 折算，
// 并标成 estimated。
func (b *Bin) MeasureSyntheticChain(ctx context.Context, enc string, dstW, dstH int) (float64, error) {
	build := func(t int) []string {
		args := []string{"-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc2=size=1920x1080:rate=24",
			"-t", strconv.Itoa(t),
			"-vf", fmt.Sprintf("scale=%d:%d", dstW, dstH), "-c:v", enc}
		args = append(args, encoderArgs(enc)...)
		args = append(args, "-b:v", "8M", "-f", "null", "-")
		return args
	}
	s, err := b.measureWithBaseline(ctx, measureSeconds, build)
	if err != nil {
		return 0, fmt.Errorf("probe: 合成源测量失败: %w", err)
	}
	return s.FPS, nil
}

// estimateFromSynthetic 把"只有编码"的吞吐折算成"含 4K 解码"的全链吞吐。
//
// 2.06 来自 R1 在同一台机器上的实测：纯编码 138 fps、全链 67 fps。
// 只在**软件编解码**时用它，因为只有那条链路是串行的（解码等编码、编码等解码）。
const estimateFromSynthetic = 2.06

// contentDiscountHW 是硬件链路的合成源折扣。
//
// 硬件解码与硬件编码跑在不同引擎上、是重叠的，所以全链吞吐约等于编码那一段，
// **不能**再除以 2.06——那等于假设解码和编码排队，而它们根本不排队。
// 实测印证：同一台机器合成源编码 435 fps，真实片源全链 476 fps，几乎相同。
//
// 剩下要打折的只有内容差异：testsrc2 是合成图形，比真实胶片的颗粒和细节好压。
// 1.25 是一个保守的经验折扣，只在没有真实片源时用，标 estimated。
// 有真实片源时这条路径根本不会走到。
const contentDiscountHW = 1.25

// estimateChainFromEncodeOnly 把"只有编码"的测量折算成全链估计。
//
// 分两条路是因为两种硬件拓扑不一样：软件编解码串行（要除以 2.06），
// 硬件编解码并行（只除以内容折扣）。用同一个系数会把一台硬件机器低估一半。
func estimateChainFromEncodeOnly(encodeOnly float64, hasHW bool) float64 {
	if encodeOnly <= 0 {
		return 0
	}
	if hasHW {
		return encodeOnly / contentDiscountHW
	}
	return encodeOnly / estimateFromSynthetic
}

// diskWriteMBps 测数据目录所在盘的写入吞吐。
//
// 只写数据目录，不写媒体目录：媒体目录是用户的库，往里丢临时文件不可接受
// （扫描器会把它当垃圾、用户会看见莫名文件）。数据目录是我们自己的，
// SQLite 的 WAL 也在这里，这个数对"扫描并发开多大"才有直接意义。
func diskWriteMBps(dir string, mb int) float64 {
	if mb <= 0 {
		return 0
	}
	f, err := os.Create(filepath.Join(dir, ".probe-disk.tmp"))
	if err != nil {
		return 0
	}
	name := f.Name()
	defer os.Remove(name)

	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i)
	}
	start := time.Now()
	for i := 0; i < mb; i++ {
		if _, err := f.Write(buf); err != nil {
			f.Close()
			return 0
		}
	}
	// Sync 是关键：不落盘就只是在写页缓存，测出来的是内存带宽。
	if err := f.Sync(); err != nil {
		f.Close()
		return 0
	}
	elapsed := time.Since(start)
	f.Close()
	if elapsed <= 0 {
		return 0
	}
	return float64(mb) / elapsed.Seconds()
}

// targetHeight 按目标宽高比算出偶数高度。
func targetHeight(srcW, srcH, dstW int) int {
	if srcW <= 0 || srcH <= 0 {
		return 0
	}
	h := int(math.Round(float64(srcH) * float64(dstW) / float64(srcW)))
	if h%2 != 0 {
		h++
	}
	if h < 2 {
		h = 2
	}
	return h
}

// seekPoint 选一个取样点。
//
// 从 10% 处开始而不是从 0：片头往往是黑场或静态 logo，编码器对它几乎不花时间，
// 测出来的帧率会虚高。取样点还要留够窗口的余量，否则短片段只解出几帧。
func seekPoint(durationS float64, secs int) int {
	if durationS <= float64(secs)*3 {
		return 0
	}
	s := int(durationS * 0.1)
	if s > 600 {
		s = 600
	}
	if float64(s+secs) > durationS {
		s = int(durationS) - secs - 1
	}
	if s < 0 {
		s = 0
	}
	return s
}

// secondSeek 选第二个取样点。
//
// 取靠后的位置而不是紧邻第一个：紧邻的两段在同一场景里，测出来几乎一样，
// 白花一倍时间还得不到第二个样本。隔开半部片子才能碰到不同的场景类型。
func secondSeek(durationS float64, secs, first int) int {
	if durationS <= float64(secs)*3 {
		return 0
	}
	s := first + int(durationS*0.45)
	if float64(s+secs) > durationS {
		s = int(durationS) - secs - 1
	}
	if s <= first {
		return 0
	}
	return s
}
