// Command lumen 是 Lumen 的自托管媒体库服务端。
//
// 当前阶段：S1（store + 迁移 + 路径归一化）。HTTP 层尚未接入，
// 所以本命令只做装配与自检：打开库、跑迁移、打印版本后退出。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Chenglp/lumen/internal/domain"
	"github.com/Chenglp/lumen/internal/pipeline"
	"github.com/Chenglp/lumen/internal/probe"
	"github.com/Chenglp/lumen/internal/store"
)

// 版本信息在构建时注入：-ldflags "-X main.version=..."
var (
	version   = "0.1.0-dev"
	commit    = "none"
	buildTime = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "lumen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dataDir     = flag.String("data", defaultDataDir(), "数据目录（lumen.db 所在处，必须在本地盘）")
		showVer     = flag.Bool("version", false, "打印版本后退出")
		migrateOnly = flag.Bool("migrate", false, "只执行迁移后退出")
		scanDir     = flag.String("scan", "", "注册该目录为媒体库并立即扫描一次，然后退出")
		libKind     = flag.String("kind", "movie", "媒体库类型：movie | tv | photo | music")
		libName     = flag.String("name", "", "媒体库显示名，默认取目录名")
		doProbe     = flag.Bool("probe", false, "探测本机转码能力并落库，然后退出")
		probeSample = flag.String("sample", "", "探测用的片源（不给则取库里最高/最大的一个）")
		probeFast   = flag.Bool("probe-fast", false, "快速探测：跳过 4K 与 DV tonemap 两项")
		probeSoft   = flag.Bool("probe-soft", false, "强制用软编探测（验证 M/L 档分支）")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lumen %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbPath := filepath.Join(*dataDir, "lumen.db")
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		return fmt.Errorf("数据库自检失败: %w", err)
	}

	applied, err := db.Migrate(ctx)
	if err != nil {
		return err
	}
	for _, v := range applied {
		fmt.Printf("migrate: 已应用 %s\n", v)
	}

	schemaVer, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("lumen %s | db=%s | schema=%s | 本次迁移 %d 个\n",
		version, dbPath, schemaVer, len(applied))

	if *scanDir != "" {
		return runScan(ctx, db, *scanDir, *libKind, *libName)
	}

	if *doProbe {
		return runProbe(ctx, db, *dataDir, *probeSample, *probeFast, *probeSoft)
	}

	if *migrateOnly {
		return nil
	}

	// S1 阶段没有长驻循环；这里留出装配点，S2 起接 HTTP/WS 与 pipeline。
	fmt.Println("服务端尚未接入 HTTP 层（S2）。按 Ctrl-C 退出。")
	<-ctx.Done()
	fmt.Println("已退出。")
	return nil
}

// runScan 注册一个媒体库并扫描一次。
//
// 这是 S1 的验证入口，也是 S2 的 `POST /libraries` + `POST /libraries/{id}/scan`
// 的同样路径：拆挂载点 → 取卷身份 → 幂等注册 → 扫描。差别只在于
// 正式链路会把扫描放进任务队列并推 WS 进度，这里直接同步跑。
func runScan(ctx context.Context, db *store.DB, dir, kind, name string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("解析路径 %s: %w", dir, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("目录不可用 %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s 不是目录", abs)
	}

	mount, rootRel, ok := domain.SplitMount(abs)
	if !ok {
		return fmt.Errorf("%s 不是可识别的绝对路径", abs)
	}
	volKey, err := domain.VolumeKeyOf(abs)
	if err != nil {
		return fmt.Errorf("确定 %s 所在卷: %w", abs, err)
	}

	vol, volNew, err := db.EnsureVolume(ctx, volKey, "local", mount)
	if err != nil {
		return err
	}
	lib, libNew, err := db.EnsureLibrary(ctx, vol.ID, rootRel, kind, name)
	if err != nil {
		return err
	}

	fmt.Printf("卷  : %s  key=%s  mount=%s  %s\n", vol.ID, vol.VolumeKey, mount, newTag(volNew))
	fmt.Printf("库  : %s  root=%s  kind=%s  %s\n", lib.ID, lib.RootRel, lib.Kind, newTag(libNew))

	// 已入库的库第一次扫是「首次设置」，不是「增量扫描」。
	firstScan := lib.LastScanAt == 0

	sc := pipeline.NewScanner(db)
	start := time.Now()
	res, err := sc.ScanLibrary(ctx, lib.ID, func(p pipeline.Progress) {
		fmt.Printf("\r扫描中 … %d 个文件", p.Done)
	})
	fmt.Printf("\r")
	dur := time.Since(start)

	if err != nil {
		return fmt.Errorf("扫描失败: %w", err)
	}

	fmt.Printf("耗时  : %s", dur.Round(time.Millisecond))
	if res.Scanned > 0 {
		fmt.Printf("  (%.0f 文件/秒)", float64(res.Scanned)/dur.Seconds())
	}
	fmt.Println()
	fmt.Printf("扫描  : %d 个视频文件\n", res.Scanned)
	fmt.Printf("结果  : 新增 %d / 变化 %d / 未变 %d / 消失 %d / 恢复 %d\n",
		res.Added, res.Updated, res.Unchanged, res.Removed, res.Restored)
	if res.Partial {
		fmt.Println("注意  : 遍历未完整结束，本次不做消失判定")
	}
	if firstScan && res.Scanned == 0 {
		fmt.Println("提示  : 目录里没有认出任何视频文件，检查扩展名或库里是不是还有子目录")
	}
	return nil
}

// runProbe 探测本机转码能力并落库。
//
// 样本的选择规则本身就带着一个判断：**优先用最高、最大的那个文件**。
// 4K HEVC 10bit 是最难的解码组合，用它测出来的数与其它片源相比是保守的；
// 反过来拿一部 720p 老片测，会把一台本来只能勉强转 1080p 的机器估成"轻松"，
// 然后在真实片源上掉帧——这是最难受的一类错误，因为它在验收时不会出现。
func runProbe(ctx context.Context, db *store.DB, dataDir, sample string, fast, forceSoft bool) error {
	var candidates []probe.Sample
	if sample == "" {
		// 先找已入库的（有真实高度，挑得准）；S1 阶段库里还没有 media 行，
		// 回退到 unmatched——扫描过但还没匹配的文件，高度未知，只能按大小挑。
		if p, _, h, size, err := db.LargestMediaFile(ctx); err == nil {
			candidates = append(candidates, probe.Sample{Path: p, Height: h, SizeBytes: size})
		} else if p, size, err := db.LargestUnmatchedFile(ctx); err == nil {
			candidates = append(candidates, probe.Sample{Path: p, SizeBytes: size})
		}
	}

	fmt.Println("探测本机转码能力 …")
	p := probe.Run(ctx, probe.Options{
		Sample:        sample,
		Candidates:    candidates,
		DataDir:       dataDir,
		Fast:          fast,
		ForceSoftware: forceSoft,
		Logf: func(format string, args ...any) {
			fmt.Printf("  %s\n", fmt.Sprintf(format, args...))
		},
	})

	fmt.Println()
	fmt.Println("── 探测结果 ─────────────────────────────")
	fmt.Print(probe.Report(p))

	if err := db.SaveHostProfile(ctx, p); err != nil {
		return err
	}
	fmt.Printf("\n已写入 host_profile（耗时 %d ms）\n", p.ProbeMS)
	if p.SampleKind != "real" {
		fmt.Println("提示: 本次没有真实片源样本，吞吐是折算出来的估计值；扫描入库后请重新探测。")
	}
	return nil
}

func newTag(created bool) string {
	if created {
		return "[新建]"
	}
	return "[已存在]"
}

// defaultDataDir 返回默认数据目录。
//
// 硬要求：媒体卷与数据目录必须分开配置（框架 §1.1）。
// lumen.db 只能放在本地盘——SQLite 的 WAL 依赖 POSIX 文件锁与 mmap 一致性，
// 放在 SMB/NFS 上会静默损坏，不是报错而是数据坏掉。
func defaultDataDir() string {
	if v := os.Getenv("LUMEN_DATA_DIR"); v != "" {
		return v
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return filepath.Join(base, "Lumen")
}
