package domain

import (
	"fmt"
	"math"
	"strings"
)

// 主机能力分档（框架 §1.1）。
//
// 这里的关键变化来自 R1 实测：**倍率是相对帧率的，吞吐才是绝对的**。
// 所以分档判据统一用「每秒能处理多少帧」，再按基准帧率 24 折算成倍率；
// 库里存的也是 fps，倍率只在展示时现算。
const (
	// refFPS 是折算倍率的基准帧率。选 24 而不是 30/60：
	// 电影绝大多数是 23.976/24，拿它做基准得到的是"最乐观情况下的倍率"，
	// 遇到 HFR 片源时算出来的倍率会自然下降——这正是我们想要的保守方向。
	refFPS = 24.0

	// perStreamFactor 是每路转码预留的余量。
	//
	// 实测 fps 是"纯视频转码"的理想值：没有音轨转码、没有 mux、没有磁盘抖动、
	// 没有别的东西在抢 CPU。真实播放时这些都要吃一点，所以并发放行时
	// 按 1.35 倍需求计算，而不是 1.0 倍。这个系数的方向是保守的：
	// 宁可少放一路，也不要放到第二路开始掉帧。
	perStreamFactor = 1.35

	// 分档阈值，单位 fps（= 基准倍率 × 24）。
	tierSFPS = refFPS * 2.0 // 48 fps，2× 实时
	tierMFPS = refFPS * 1.0 // 24 fps，1× 实时
	tierLFPS = refFPS * 0.3 // 7.2 fps，0.3× 实时
	// 0.3–1.0 之间没有单列一档：框架原文 S/M/L 的区间在 0.6–1.0 处是断开的，
	// 这里把 0.6–1.0 并入 L（保守），避免出现"落不进任何档"的空洞。

	maxConcS = 3
	maxConcM = 2
	maxConcL = 1
	maxConcX = 1 // X 档只允许 remux，不转码；留着 1 是为了让队列不是死锁
)

// EncoderInfo 是一个编码器的可用性判定。
//
// Listed 与 Verified 必须分开：`ffmpeg -encoders` 里有名字，不等于真能用。
// 虚拟机、远程桌面会话、驱动装了但卡不在、消费卡并发授权不足，
// 都会让"列表里有"变成"一跑就失败"。只有真编过一小段才能置 Verified。
type EncoderInfo struct {
	Name     string `json:"name"`
	Listed   bool   `json:"listed"`
	Usable   bool   `json:"usable"`
	Verified bool   `json:"verified"`
	Hardware bool   `json:"hardware"`
	Why      string `json:"why,omitempty"`
}

// HostMeasurements 是探测的原始产出，不含任何推论。
//
// 与 HostProfile 分开是因为这两件事的正确性来源不同：
// 测量只可能"测错"，决策只可能"算错"。分开之后，
// 决策可以拿固定的测量值做表驱动测试，不需要真的起 ffmpeg。
type HostMeasurements struct {
	HasFFmpeg      bool     `json:"hasFfmpeg"`
	FFmpegPath     string   `json:"ffmpegPath,omitempty"`
	FFmpegVersion  string   `json:"ffmpegVersion,omitempty"`
	FFmpegFeatures []string `json:"ffmpegFeatures,omitempty"`

	Encoders     []EncoderInfo `json:"encoders,omitempty"`
	HWEncoder    string        `json:"hwEncoder,omitempty"`    // 选中的硬编，空表示只有软编
	SoftEncoder  string        `json:"softEncoder,omitempty"`  // 实际用于测量的编码器
	DecodeFPS    float64       `json:"decodeFps,omitempty"`    // 仅解码
	ChainFPS1080 float64       `json:"chainFps1080,omitempty"` // 4K→1080p 全链
	ChainFPS2160 float64       `json:"chainFps2160,omitempty"` // 4K→2160p 全链
	TonemapFPS   float64       `json:"tonemapFps,omitempty"`   // DV→SDR 全链

	SampleKind   string `json:"sampleKind,omitempty"` // real | synthetic | none
	SamplePath   string `json:"samplePath,omitempty"`
	SampleHeight int    `json:"sampleHeight,omitempty"`
	SampleHDR    string `json:"sampleHdr,omitempty"` // sdr | hdr10 | hlg | dv5 | dv7 | dv8

	Cores         int     `json:"cores,omitempty"`
	MemMB         int     `json:"memMb,omitempty"`
	OnBattery     bool    `json:"onBattery,omitempty"`
	DiskClass     string  `json:"diskClass,omitempty"` // local | removable | network
	DiskWriteMBps float64 `json:"diskWriteMbps,omitempty"`

	ProbeMS int64  `json:"probeMs,omitempty"`
	Err     string `json:"err,omitempty"`
}

// HostProfile 是落库并驱动运行时决策的最终结果。
type HostProfile struct {
	HostMeasurements

	Tier                 string   `json:"tier"` // S | M | L | X
	MaxHeight            int      `json:"maxHeight"`
	TranscodeConcurrency int      `json:"transcodeConcurrency"`
	TonemapSlots         int      `json:"tonemapSlots"`
	Confidence           string   `json:"confidence"` // measured | estimated
	Reasons              []string `json:"reasons,omitempty"`
	ProbedAt             int64    `json:"probedAt"`
}

// DecideHost 把测量值折算成档位、并发数、档位上限。
//
// 纯函数：不读盘、不起进程、不看时间。所有"为什么这么算"都留在注释里，
// 因为这套阈值一旦被后人当成魔法数字随手调，掉帧就会变成偶发投诉。
func DecideHost(m HostMeasurements) HostProfile {
	p := HostProfile{HostMeasurements: m}
	add := func(format string, args ...any) {
		p.Reasons = append(p.Reasons, fmt.Sprintf(format, args...))
	}

	// 没有 ffmpeg 就等于没有转码能力。这不区分"装在哪"，只看找没找到。
	if !m.HasFFmpeg {
		p.Tier, p.MaxHeight, p.TranscodeConcurrency, p.TonemapSlots = "X", 720, 1, 0
		p.Confidence = "estimated"
		// 原因必须落到 profile 上：32 屏的本机诊断要显示"为什么不能转码"。
		// 只给一个 X 档字母，用户看到的是无法行动的结论。
		if p.Err == "" {
			p.Err = "找不到 ffmpeg，转码能力不可用"
		}
		add("找不到 ffmpeg，按 X 档只做 remux")
		return p
	}

	// 全链吞吐是唯一可信的决策依据。
	//
	// 为什么不用「解码 fps + 编码 fps 合成」：那只在**串行**链路上成立。
	// 用硬件解码 + 硬件编码时两者跑在不同引擎上、是重叠的——R1 实测
	// 解码 137 fps、全链 254 fps，全链比解码还快，串行模型直接算出负数。
	// 所以这里只认直接测出来的全链吞吐，两侧都是保守的：
	// 1080p 源比 4K 源好解，720p 目标比 1080p 好编，而我们的数取自最难的组合。
	chain := m.ChainFPS1080
	if chain <= 0 {
		p.Tier, p.MaxHeight, p.TranscodeConcurrency, p.TonemapSlots = "X", 720, 1, 0
		p.Confidence = "estimated"
		if p.Err == "" {
			p.Err = "全链吞吐测不出来，转码能力按不可用处理"
		}
		add("全链吞吐测不出来，按 X 档只做 remux")
		return p
	}

	ratio := chain / refFPS
	hw := m.HWEncoder != ""

	switch {
	case hw && ratio >= 2:
		p.Tier = "S"
	case hw || ratio >= 1:
		p.Tier = "M"
	case chain >= tierLFPS:
		p.Tier = "L"
	default:
		p.Tier = "X"
	}

	switch p.Tier {
	case "S":
		p.MaxHeight = 2160
	case "M", "L":
		p.MaxHeight = 1080
	default:
		p.MaxHeight = 720
	}

	// 并发由吞吐推导，不由"卡允许几路"推导。
	// R1 实测 5 路 NVENC 同开没有任何会话数报错，且聚合吞吐上升；
	// 消费卡的会话限制在这代驱动上不构成约束。真正的约束是每路还剩多少余量。
	conc := int(math.Floor(chain / (refFPS * perStreamFactor)))
	if conc < 1 {
		conc = 1
	}
	max := maxConcL
	switch p.Tier {
	case "S":
		max = maxConcS
	case "M":
		max = maxConcM
	case "X":
		max = maxConcX
	}
	if conc > max {
		conc = max
	}
	p.TranscodeConcurrency = conc

	// tonemap 槽位硬限 1。
	//
	// 这不是保守，是实测：DV→SDR 的 tonemap 是逐像素成本，与输出分辨率无关
	// （720p 72.1 fps vs 1080p 69.4 fps 几乎一样），一部 60fps 的 DV 片子
	// 就只剩 1.18×，第二路必然掉帧。放宽到 2 只会在"两个人在看 DV"时一起卡。
	if m.TonemapFPS > 0 {
		p.TonemapSlots = 1
		add("tonemap 实测 %.1f fps，槽位锁 1 路", m.TonemapFPS)
	} else if hasFeature(m.FFmpegFeatures, "libplacebo") {
		// 有 libplacebo 但没测出 tonemap：可能是没有 DV 样本。
		// 仍然给 1 路——只是起播时会边走边看，不会预先拒绝。
		p.TonemapSlots = 1
		add("有 libplacebo 但无 DV 样本，tonemap 槽位按 1 路预留")
	} else {
		p.TonemapSlots = 0
		add("ffmpeg 缺少 libplacebo，DV P5 无法转 SDR（只能直连或预转）")
	}

	p.Confidence = "measured"
	if m.SampleKind != "real" {
		p.Confidence = "estimated"
		add("无真实片源样本，吞吐按合成源测量，偏乐观")
	}

	if m.OnBattery {
		// C4：电池上不该跑转码。档位不降，只标记——降档会把"这台机器行不行"
		// 和"现在这一刻方不方便"混成一个数，之后没法解释。
		add("当前在电池供电，转码应暂停而非降档")
	}

	add("全链 4K→1080p %.1f fps（%.1f× @24fps），高 %d 路", chain, ratio, p.TranscodeConcurrency)
	return p
}

// ChainFPSFor 返回目标高度下的全链吞吐估计。
//
// 只升不降地取已测值：720p 目标会借用 1080p 的数（更省，真实值只会更高）。
// 反过来不行——2160p 目标没有实测就退到 1080p 的数，这是保守方向。
func (p HostProfile) ChainFPSFor(height int) float64 {
	if height >= 2160 && p.ChainFPS2160 > 0 {
		return p.ChainFPS2160
	}
	return p.ChainFPS1080
}

// RatioAt 返回给定片源帧率下的实时倍率。
//
// 播放决策用它，而不是用一个"这台机器 4×"的常数：同一台机器在 24fps 片源上
// 是 10×，在 60fps 的 HFR 片源上只剩 4×。拿倍率当机器属性就会在这里出错。
func (p HostProfile) RatioAt(srcFPS float64, height int) float64 {
	if srcFPS <= 0 {
		srcFPS = refFPS
	}
	fps := p.ChainFPSFor(height)
	if fps <= 0 {
		return 0
	}
	return fps / srcFPS
}

// CanServeHeight 判断某个档位是否允许出现在 renditions 里。
//
// 契约 §4.1 的硬要求：L 档机器不能把 2160 放进响应，否则用户点了就是
// 卡顿或 TRANSCODER_BUSY。档位过滤发生在服务端，不是客户端隐藏按钮。
func (p HostProfile) CanServeHeight(height int) bool { return height <= p.MaxHeight }

// NeedsProxy 判断是否该走 B 分支（预转代理版）。
//
// 判据不是"档位低"，而是"实时转码已经做不到 1× 了"。
// 一台 L 档机器转 1080p 源仍然可以实时，只有 4K 源才需要预先转。
func (p HostProfile) NeedsProxy(srcHeight int, srcFPS float64) bool {
	return p.RatioAt(srcFPS, 1080) < 1.0
}

func hasFeature(features []string, want string) bool {
	for _, f := range features {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}
