package probe

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Chenglp/lumen/internal/domain"
)

// Sample 是一个可用的测量样本。
type Sample struct {
	Path      string
	Height    int
	SizeBytes int64
}

// Options 控制一次探测。
type Options struct {
	// Sample 显式指定样本；空则表示从 Candidates 里挑。
	Sample string
	// Candidates 一般来自库里已探测过的媒体。挑最高的那个而不是第一个：
	// 4K HEVC 10bit 是最难解的组合，用它测出来的数是保守值；
	// 拿一部 720p 老片测会让整台机器显得很强，然后在真实片源上掉帧。
	Candidates []Sample
	// DataDir 用于测磁盘写入。空则跳过该项。
	DataDir string
	// Fast 跳过 2160 与 tonemap 两项，用于"只想快速知道大概"的场景。
	Fast bool
	// ForceSoftware 强制用软编测量。
	//
	// 不是给用户用的开关，是给"验证 M/L 档分支"用的：一台有硬编的机器
	// 永远只会被分到 S 档，M/L/X 三条分支就永远没在真机上跑过。
	// 强制软编可以在同一台机器上得到一次真实的 M 档结果。
	ForceSoftware bool
	// Budget 是总时间预算。超时不算失败，已测到的数照常返回。
	Budget time.Duration
	// Logf 接收进度文本，可为 nil。
	Logf func(format string, args ...any)
}

// Run 执行一次完整探测，返回可直接落库的 HostProfile。
//
// 这个函数**不返回 error**：探测失败是一种结果（X 档），不是异常。
// 没有 ffmpeg 的机器照样要能启动、能扫描、能直连播放，只是不能转码。
// 把失败变成 error 逼调用方处理，最后一定会被写成 `if err != nil { return }`，
// 结果就是服务起不来——而用户只是想放个片子。
func Run(ctx context.Context, opts Options) domain.HostProfile {
	start := time.Now()
	budget := opts.Budget
	if budget <= 0 {
		budget = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}

	secs := measureSeconds
	if opts.Fast {
		secs = fastMeasureSeconds
	}

	var m domain.HostMeasurements
	m.Cores = cpuCount()
	m.MemMB = memTotalMB()
	m.OnBattery = onBattery()

	bin, err := Find()
	if err != nil {
		m.Err = "找不到 ffmpeg，转码能力不可用"
		logf("ffmpeg: 未找到")
		return finish(m, start)
	}
	m.HasFFmpeg = true
	m.FFmpegPath = bin.FFmpeg
	m.FFmpegVersion = bin.Version(ctx)
	logf("ffmpeg: %s", m.FFmpegVersion)

	if feats, ferr := bin.Features(ctx); ferr == nil {
		m.FFmpegFeatures = feats
	}

	// ── 编码器：先看列表，再真编 ────────────────────────────────
	listed, _ := bin.Encoders(ctx)
	m.Encoders = verifyEncoders(ctx, bin, listed, logf)
	m.HWEncoder, m.SoftEncoder = pickEncoders(m.Encoders)
	if m.HWEncoder == "" && m.SoftEncoder == "" {
		m.Err = "没有可用的视频编码器"
		logf("编码器: 全部不可用")
		return finish(m, start)
	}
	enc := m.HWEncoder
	if enc == "" {
		enc = m.SoftEncoder
	}
	if opts.ForceSoftware && m.SoftEncoder != "" {
		enc = m.SoftEncoder
		// 清掉硬编标记：档位判断看的是"实际走哪条路"，
		// 留着 hw 标记会让一次软编测量被判成 S 档。
		m.HWEncoder = ""
		logf("编码器: 强制使用软编 %s（用于验证 M/L 档分支）", enc)
	} else {
		logf("编码器: 使用 %s", enc)
	}

	// ── 样本 ───────────────────────────────────────────────────
	samplePath, kind := pickSample(opts)
	m.SampleKind = kind
	m.SamplePath = samplePath
	logf("样本: %s", describeSample(kind, samplePath))

	if kind != "real" {
		// 没有真实片源：只能测"缩放 + 编码"这一段，再按实测系数折算。
		// 这条路存在的意义是"空库的新装也能给出合理档位"，不是"给出准确档位"，
		// 所以结果一律标 estimated，并在 CLI 里提示加库后重测。
		fps, e := bin.MeasureSyntheticChain(ctx, enc, 1920, 1080)
		if e != nil {
			m.Err = "合成源测量失败: " + e.Error()
		} else {
			m.ChainFPS1080 = estimateChainFromEncodeOnly(fps, m.HWEncoder != "")
			logf("合成源 1080p 编码 %.1f fps → 折算全链 %.1f fps（硬编=%v）",
				fps, m.ChainFPS1080, m.HWEncoder != "")
		}
		m.DiskClass, m.DiskWriteMBps = measureDisk(opts.DataDir)
		return finish(m, start)
	}

	info, err := bin.probeSample(ctx, samplePath)
	if err != nil {
		// 有文件但读不出参数（损坏、编码不支持）：退回合成源路径，
		// 而不是直接判 X。一部坏文件不该让整台机器降档。
		logf("样本: 读不出参数（%v），退回合成源", err)
		m.SampleKind = "synthetic"
		if fps, e2 := bin.MeasureSyntheticChain(ctx, enc, 1920, 1080); e2 == nil {
			m.ChainFPS1080 = estimateChainFromEncodeOnly(fps, m.HWEncoder != "")
		}
		m.DiskClass, m.DiskWriteMBps = measureDisk(opts.DataDir)
		return finish(m, start)
	}
	m.SampleHeight = info.Height
	m.SampleHDR = info.HDR
	logf("样本参数: %dx%d %.3gfps %s %s", info.Width, info.Height, info.FPS, info.PixelFormat, info.HDR)

	seek := seekPoint(info.DurationS, secs)
	// 全链测量取两个取样点、用较小的那个：同一台机器同一命令在不同片段上
	// 能差 13%（动作戏比对话戏难压）。只测一个点等于在赌场景，取最小值是保守方向。
	seeks := []int{seek}
	if !opts.Fast {
		if s2 := secondSeek(info.DurationS, secs, seek); s2 > 0 {
			seeks = append(seeks, s2)
		}
	}
	if fps, e := bin.MeasureDecode(ctx, samplePath, seek, secs); e == nil {
		m.DecodeFPS = fps
		logf("解码: %.1f fps", fps)
	}

	dstW, dstH := 1920, targetHeight(info.Width, info.Height, 1920)
	chain, plan, err := bin.MeasureChain(ctx, samplePath, seeks, secs, enc, dstW, dstH)
	if err != nil {
		m.Err = "全链测量失败: " + err.Error()
		logf("全链 4K→1080p: 失败 %v", err)
		m.DiskClass, m.DiskWriteMBps = measureDisk(opts.DataDir)
		return finish(m, start)
	}
	m.ChainFPS1080 = chain
	logf("全链 4K→1080p: %.1f fps（%s 解码）", chain, plan)

	// 2160 档只有源本身够高才有意义：拿 1080p 样本"升采样到 4K"测出来的
	// 不是用户会遇到的场景，反而白花十几秒。
	if !opts.Fast && info.Height >= 1600 {
		// 2160 档只用第一个取样点：它只决定"能不能实时转 4K"，
		// 而且这一项本来就慢，多测一个点会让探测时间再涨 8 秒。
		if fps, _, e := bin.MeasureChain(ctx, samplePath, []int{seek}, secs, enc,
			info.Width, info.Height); e == nil {
			m.ChainFPS2160 = fps
			logf("全链 原分辨率→%dp: %.1f fps", info.Height, fps)
		}
	}

	// DV P5 是唯一"画面错了但不报错"的情形，必须单独测一条转 SDR 的链路。
	// 只有存在 libplacebo 时才测——没有它这条链路根本不存在。
	if !opts.Fast && info.IsDolbyVisionProfile5() && featureHas(m.FFmpegFeatures, "libplacebo") {
		if fps, e := bin.MeasureTonemap(ctx, samplePath, seek, secs, dstW, dstH, enc); e == nil {
			m.TonemapFPS = fps
			logf("DV→SDR 1080p: %.1f fps", fps)
		} else {
			logf("DV→SDR: 失败 %v", e)
		}
	}

	m.DiskClass, m.DiskWriteMBps = measureDisk(opts.DataDir)
	return finish(m, start)
}

// finish 补齐耗时并做决策。
//
// 耗时必须在返回**之前**填进结构体：写在 defer 里改局部变量是无效的，
// 因为返回值在 defer 执行前就已经算好了——那样 ProbeMS 会永远是 0，
// 而且不会有任何报错。
func finish(m domain.HostMeasurements, start time.Time) domain.HostProfile {
	m.ProbeMS = time.Since(start).Milliseconds()
	p := domain.DecideHost(m)
	p.ProbedAt = start.UnixMilli()
	return p
}

// verifyEncoders 对候选编码器逐个真编。
//
// 只验证"列表里有的"，且最多验证前 maxHWVerify 个硬件候选：
// 每验证一个要起一次 ffmpeg，全验一遍会让首次启动的探测时间翻倍，
// 而"第 4 个硬件编码器可用、前 3 个都不可用"这种情形基本不存在。
func verifyEncoders(ctx context.Context, b *Bin, listed map[string]bool,
	logf func(string, ...any)) []domain.EncoderInfo {

	const maxHWVerify = 3
	var out []domain.EncoderInfo
	hwTried := 0

	for _, name := range hwCandidates {
		if !listed[name] || hwTried >= maxHWVerify {
			continue
		}
		hwTried++
		out = append(out, verifyOne(ctx, b, name, true, logf))
	}
	for _, name := range softCandidates {
		if !listed[name] {
			continue
		}
		out = append(out, verifyOne(ctx, b, name, false, logf))
	}
	return out
}

func verifyOne(ctx context.Context, b *Bin, name string, hardware bool,
	logf func(string, ...any)) domain.EncoderInfo {

	info := domain.EncoderInfo{Name: name, Listed: true, Hardware: hardware}
	ok, why := b.verifyEncoder(ctx, name)
	info.Usable, info.Verified = ok, ok
	if !ok {
		info.Why = why
	}
	logf("编码器: %-18s usable=%v", name, ok)
	return info
}

// pickEncoders 从验证结果里挑出实际要用的硬编与软编。
func pickEncoders(list []domain.EncoderInfo) (hw, soft string) {
	for _, e := range list {
		if !e.Usable {
			continue
		}
		if e.Hardware && hw == "" {
			hw = e.Name
		}
		if !e.Hardware && soft == "" {
			soft = e.Name
		}
	}
	return hw, soft
}

// pickSample 决定测量样本。
func pickSample(opts Options) (path, kind string) {
	if opts.Sample != "" {
		if fi, err := os.Stat(opts.Sample); err == nil && !fi.IsDir() {
			return opts.Sample, "real"
		}
	}
	best := Sample{}
	for _, c := range opts.Candidates {
		if c.Path == "" {
			continue
		}
		if c.Height > best.Height || (c.Height == best.Height && c.SizeBytes > best.SizeBytes) {
			best = c
		}
	}
	if best.Path != "" {
		if fi, err := os.Stat(best.Path); err == nil && !fi.IsDir() {
			return best.Path, "real"
		}
	}
	return "", "none"
}

func describeSample(kind, path string) string {
	if kind == "real" {
		return path
	}
	return "无真实片源，使用合成源"
}

// measureDisk 测数据目录所在的盘。
func measureDisk(dir string) (class string, mbps float64) {
	if dir == "" {
		return "unknown", 0
	}
	if isNetworkPath(dir) {
		// 网络盘上跑一次写入测试要几十秒，测出来的还是链路带宽不是盘。
		// 直接按路径类型判，写入吞吐留 0 表示"没测"。
		return "network", 0
	}
	mbps = diskWriteMBps(dir, 32)
	switch {
	case mbps <= 0:
		return "unknown", 0
	case mbps < 150:
		return "hdd", mbps
	default:
		return "ssd", mbps
	}
}

func isNetworkPath(p string) bool {
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return true
	}
	return isNetworkDrive(p)
}

func featureHas(features []string, want string) bool {
	for _, f := range features {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}

// Report 把探测结果整理成给人看的几行。
func Report(p domain.HostProfile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "档位      %s（最高 %dp，并发 %d 路，tonemap %d 路）\n",
		p.Tier, p.MaxHeight, p.TranscodeConcurrency, p.TonemapSlots)
	fmt.Fprintf(&b, "置信度    %s（样本 %s）\n", p.Confidence, emptyAs(p.SampleKind, "无"))
	if p.ChainFPS1080 > 0 {
		fmt.Fprintf(&b, "全链吞吐  4K→1080p %.1f fps = %.1f× @24fps\n",
			p.ChainFPS1080, p.ChainFPS1080/24)
	}
	if p.ChainFPS2160 > 0 {
		fmt.Fprintf(&b, "          →2160p   %.1f fps = %.1f× @24fps\n",
			p.ChainFPS2160, p.ChainFPS2160/24)
	}
	if p.DecodeFPS > 0 {
		fmt.Fprintf(&b, "仅解码    %.1f fps\n", p.DecodeFPS)
	}
	if p.TonemapFPS > 0 {
		fmt.Fprintf(&b, "DV→SDR    %.1f fps\n", p.TonemapFPS)
	}
	fmt.Fprintf(&b, "硬件编码  %s；软件编码 %s\n", emptyAs(p.HWEncoder, "无"), emptyAs(p.SoftEncoder, "无"))
	fmt.Fprintf(&b, "主机      %d 核 / %d MB", p.Cores, p.MemMB)
	if p.OnBattery {
		b.WriteString("；**电池供电**")
	}
	b.WriteString("\n")
	if p.DiskClass != "" {
		fmt.Fprintf(&b, "数据盘    %s", p.DiskClass)
		if p.DiskWriteMBps > 0 {
			fmt.Fprintf(&b, "（写入 %.0f MB/s）", p.DiskWriteMBps)
		}
		b.WriteString("\n")
	}
	if p.Err != "" {
		fmt.Fprintf(&b, "注意      %s\n", p.Err)
	}
	for _, r := range p.Reasons {
		fmt.Fprintf(&b, "  · %s\n", r)
	}
	return b.String()
}

func emptyAs(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
