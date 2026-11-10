// Command lumen 是 Lumen 的自托管媒体库服务端。
//
// 当前阶段：S1（store + 迁移 + 路径归一化）。HTTP 层尚未接入，
// 所以本命令只做装配与自检：打开库、跑迁移、打印版本后退出。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Chenglp/lumen/internal/domain"
	"github.com/Chenglp/lumen/internal/pipeline"
	"github.com/Chenglp/lumen/internal/probe"
	"github.com/Chenglp/lumen/internal/store"
	"github.com/Chenglp/lumen/internal/tmdb"
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
		parseArg    = flag.String("parse", "", "只跑文件名解析：目录则遍历其中视频文件，其它值当作文件名，解析后退出")
		matchArg    = flag.String("match", "", "对目录里的视频跑一遍解析 + TMDB 匹配，打印候选后退出")
		matchMax    = flag.Int("match-max", 8, "匹配时最多处理多少个文件（每个文件至少 1 次请求）")
		matchLimit  = flag.Int("match-limit", 3, "每个文件最多显示几个候选")
		matchLang   = flag.String("lang", "zh-CN", "刮削语言，如 zh-CN / en-US")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("lumen %s (commit %s, built %s)\n", version, commit, buildTime)
		return nil
	}

	// -parse 不碰数据库：解析是纯函数，验证它不该要求先建库。
	if *parseArg != "" {
		return runParse(*parseArg)
	}

	if *matchArg != "" {
		return runMatch(*dataDir, *matchArg, *libKind, *matchLang, *matchLimit, *matchMax)
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

// runParse 打印文件名解析结果。
//
// 存在的理由和 -probe 一样：解析器的输出直接决定 28 屏每行副标与搜索词，
// 而真实库里的名字千奇百怪。拿真目录跑一遍，比在单测里编例子可信得多。
func runParse(arg string) error {
	fi, err := os.Stat(arg)
	if err != nil || !fi.IsDir() {
		// 不是目录就当成一条文件名（可以带引号传整串）
		printParsed(arg, domain.ParsePath(arg))
		return nil
	}

	root := arg
	var n int
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 读不到的目录跳过，解析演示不该因为权限中断
		}
		if d.IsDir() {
			if p != root && domain.IsJunk(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !domain.IsVideoFile(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			rel = d.Name()
		}
		printParsed(filepath.ToSlash(rel), domain.ParsePath(filepath.ToSlash(rel)))
		n++
		return nil
	})
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("目录里没有受支持的视频文件")
	}
	return nil
}

func printParsed(rel string, r domain.ParseResult) {
	p := r.Parsed

	year := "-"
	if p.Year != nil {
		year = fmt.Sprint(*p.Year)
	}
	se := "-"
	if p.Season != nil && p.Episode != nil {
		se = fmt.Sprintf("S%02dE%02d", *p.Season, *p.Episode)
	} else if p.Season != nil {
		se = fmt.Sprintf("S%02d", *p.Season)
	} else if p.Episode != nil {
		se = fmt.Sprintf("E%02d", *p.Episode)
	} else if p.SeasonPack {
		se = "整季包"
	}

	fmt.Printf("\n%s\n", rel)
	fmt.Printf("  标题  %s\n", orDash(p.Title))
	fmt.Printf("  年份  %s   季集  %s   来源  %s\n", year, se, r.TitleFrom)
	fmt.Printf("  规格  %s / %s / %s\n", orDash(p.Resolution), orDash(p.Source), orDash(p.ReleaseGroup))
	fmt.Printf("  判定  %s\n", r.Code)
	for _, w := range r.Warnings {
		fmt.Printf("  提醒  %s %v\n", w.Code, w.Params)
	}
	if p.VideoCodec != "" || p.AudioCodec != "" || len(p.Tags) > 0 {
		fmt.Printf("  编码  %s %s %s / %v\n", p.VideoCodec, p.AudioCodec, p.AudioChannels, p.Tags)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runMatch 把「解析 → 搜索 → 打分」整条链在真实文件上跑一遍。
//
// 存在的理由和 -parse、-probe 一样：这条链的每一环都有单测，但它们都是
// 拿我手写的响应喂出来的。真机跑一遍才能回答三个测试回答不了的问题：
// 真实片名搜得到吗、年份信号在真数据上起作用吗、一条要花几次请求。
func runMatch(dataDir, root, kind, lang string, limit, max int) error {
	token := strings.TrimSpace(os.Getenv("LUMEN_TMDB_TOKEN"))
	if token == "" {
		return fmt.Errorf("未设置 LUMEN_TMDB_TOKEN（TMDB v4 Read Access Token）")
	}
	if limit <= 0 {
		limit = 3
	}
	if max <= 0 {
		max = 8
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 缓存用真库：这样反复跑同一批文件时能看出来「第二次是不是 0 请求」，
	// 而那正是 27 小时里唯一能被省掉的部分。
	db, err := store.Open(filepath.Join(dataDir, "lumen.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Migrate(ctx); err != nil {
		return err
	}

	c := tmdb.New(token, tmdb.WithLanguage(lang), tmdb.WithCache(db))

	var files []string
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != root && domain.IsJunk(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !domain.IsVideoFile(d.Name()) {
				return nil
			}
			files = append(files, p)
			if len(files) >= max {
				return filepath.SkipAll
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		files = append(files, root)
	}

	if len(files) == 0 {
		fmt.Println("没有找到受支持的视频文件")
		return nil
	}

	// 有 ffmpeg 就读容器 tag：那是最省的一层（0 次搜索，1 次详情）。
	// 读不到也不影响——三级优先级的意义就是每一层都能独立工作。
	bin, _ := probe.Find()

	var (
		totalReq   int
		totalCache int
		totalTag   int
		started    = time.Now()
	)
	for _, p := range files {
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil || fi.IsDir() == false {
			rel = filepath.Base(p)
		}
		rel = filepath.ToSlash(rel)

		q := domain.MatchQuery{Result: domain.ParsePath(rel), LibKind: kind, Lang: lang}
		var hint tmdb.Hint
		if bin != nil {
			if tags, tagErr := bin.FormatTags(ctx, p); tagErr == nil {
				hint = tmdb.HintFromTags(tags)
			}
		}
		if hint.TMDBID > 0 {
			totalTag++
		}

		t0 := time.Now()
		res, err := c.Resolve(ctx, tmdb.Lookup{Query: q, Type: kind, Hint: hint, Limit: limit, Enrich: 1})
		if err != nil {
			fmt.Printf("\n%s\n  失败  %v\n", rel, err)
			if errors.Is(err, tmdb.ErrUnavailable) || errors.Is(err, tmdb.ErrUnauthorized) {
				return fmt.Errorf("中断：%w", err) // 网络/凭据问题，继续跑只会重复失败
			}
			continue
		}
		totalReq += res.Requests
		if res.Requests == 0 {
			totalCache++
		}

		fmt.Printf("\n%s\n", rel)
		fmt.Printf("  解析  %s%s   判定 %s · 把握 %.3f\n",
			orDash(q.Result.Parsed.Title), yearSuffix(q.Result.Parsed.Year), res.Code, res.Confidence)
		flags := []string{res.Source}
		if res.Confirmed {
			flags = append(flags, "文件自带 id")
		}
		if res.YearDropped {
			flags = append(flags, "去年份重搜")
		}
		if res.Disagreement {
			flags = append(flags, "nfo 与文件名不一致")
		}
		fmt.Printf("  过程  %s · %d 请求 · %dms\n",
			strings.Join(flags, " / "), res.Requests, time.Since(t0).Milliseconds())

		for i, h := range res.Hits {
			if i >= limit {
				break
			}
			star := " "
			if h.Recommended {
				star = "★"
			}
			fmt.Printf("  %s#%d %-7d %-5s %s%s  score=%.3f\n",
				star, i+1, h.TMDBID, h.MediaType, h.Title, yearSuffix(h.Year), h.Score)
		}
		if len(res.Hits) == 0 {
			fmt.Printf("  （无候选）\n")
		}
	}

	fmt.Printf("\n%d 个文件 · %d 个带容器 tag · %d 次请求 · %d 个命中缓存 · 耗时 %s\n",
		len(files), totalTag, totalReq, totalCache, time.Since(started).Round(time.Millisecond))
	return nil
}

func yearSuffix(y *int) string {
	if y == nil {
		return ""
	}
	return fmt.Sprintf(" (%d)", *y)
}
