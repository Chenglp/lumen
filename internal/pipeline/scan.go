package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/Chenglp/lumen/internal/domain"
	"github.com/Chenglp/lumen/internal/store"
)

// ErrVolumeOffline 表示卷当前不可用（拔盘、网络断、未挂载）。
//
// 这不是"扫描失败"，是常态。调用方应当把它映射成
// task.finished{result:{code:"PARTIAL"}, needsAttention:{code:"SOURCE_UNAVAILABLE"}}，
// 而不是 task.failed —— 拔掉一块移动硬盘不该让 29 屏出现一条红色错误。
var ErrVolumeOffline = errors.New("pipeline: 卷离线")

// errVolumeGone 表示遍历途中卷消失。
//
// 它与「文件被删」的区别是整条链路上最重要的一条：卷消失时必须整体中止，
// 且**绝对不能**把没见到的文件标成 missing——否则拔一次盘，整卷观影记录被清空。
var errVolumeGone = errors.New("pipeline: 卷在扫描途中不可用")

// DefaultBatchSize 是一个事务里处理多少个文件。
//
// 框架 §4.1 定的是 200：它同时是"暂停"的最小粒度——30 屏显示
// 「已处理 8,486 / 12,480」时，中断最多丢 200 个文件的工作量。
// R4 实测给的上限是单事务 ≤ 500 行，200 留了余量。
const DefaultBatchSize = 200

// Progress 是一次进度上报。
//
// Total 为 0 表示未知：流式遍历在走完之前算不出总数。
// 调用方可以拿上一次扫描的结果估，但**不要**为了拿到总数先做一遍预遍历——
// 那在网络盘上等于把 IO 翻倍，而进度条不需要这么准。
type Progress struct {
	Done  int
	Total int
	Unit  string
}

// ScanResult 对应契约里 `SCAN_DONE` 的 params：{scanned, added, updated, removed}。
type ScanResult struct {
	LibraryID string
	Scanned   int // 见到的视频文件数
	Added     int
	Updated   int
	Unchanged int
	Removed   int // 本次被标为消失的数量
	Restored  int // 上次标记消失、这次又出现了

	// NeedsReprobe 是内容变化（size/mtime）的已入库文件，相对卷根的路径。
	//
	// 扫描只改 stat 不动 probe——ffprobe 必须在写锁外跑（R4 结论），
	// 由调用方把这份清单交给探测阶段。
	NeedsReprobe []string

	// MissingPaths 是本次判定为消失的库相对路径，给 29 屏「需处理」用。
	MissingPaths []string

	// Partial 表示遍历没走完（权限错误、目录被独占等）。
	//
	// 为 true 时**不做**消失判定：没见到不等于不存在。
	Partial bool
}

// Store 是扫描器需要的窄接口。
//
// 刻意不接收 *store.DB：这个包里的逻辑是本产品最该被单测覆盖的一段，
// 而它一旦直接持有 SQL，测试就得建库。
type Store interface {
	Library(ctx context.Context, id string) (store.Library, error)
	Volume(ctx context.Context, id string) (store.Volume, error)
	KnownFiles(ctx context.Context, keys []string) (map[string]store.KnownFile, error)
	InsertUnmatchedBatch(ctx context.Context, rows []store.UnmatchedRow) (int, error)
	TouchUnmatched(ctx context.Context, id string, size, mtime int64, resetDerived bool) error
	TouchMedia(ctx context.Context, id string, size, mtime int64) error
	ListLibraryPaths(ctx context.Context, libraryID string) ([]store.PathRef, error)
	MarkMissing(ctx context.Context, refs []store.PathRef, at int64) (int, error)
	ClearMissing(ctx context.Context, refs []store.PathRef) (int, error)
	TouchLibraryScan(ctx context.Context, libraryID string, at int64) error
	SetVolumeStatus(ctx context.Context, volumeID, status string, at int64) error
}

// MountResolver 把卷解析成当前挂载点。
type MountResolver interface {
	Resolve(v store.Volume) (string, error)
}

// HintResolver 用 volumes.mount_hint 定位挂载点。
//
// 已知缺口：它**不校验卷身份**，只确认这个路径还在、还是个目录。
// 框架 §1.2 ① 要求按 volume_key（卷序列号 / VolumeUUID / blkid UUID）校验，
// 否则「D: 上换了一块硬盘」会被当成同一个卷，扫描结果全错但不报错。
// 这一步需要平台 API，排在 S3 之前补，接口已经留好。
type HintResolver struct{}

// Resolve 实现 MountResolver。
func (HintResolver) Resolve(v store.Volume) (string, error) {
	if v.Status == "offline" {
		return "", ErrVolumeOffline
	}
	if v.MountHint == "" {
		return "", ErrVolumeOffline
	}
	fi, err := os.Stat(v.MountHint)
	if err != nil || !fi.IsDir() {
		return "", ErrVolumeOffline
	}
	return v.MountHint, nil
}

// Scanner 执行库扫描。
type Scanner struct {
	store    Store
	resolver MountResolver
	batch    int
	now      func() int64
}

// ScannerOption 配置扫描器。
type ScannerOption func(*Scanner)

// WithBatchSize 改批量大小，测试用。
func WithBatchSize(n int) ScannerOption {
	return func(s *Scanner) {
		if n > 0 {
			s.batch = n
		}
	}
}

// WithClock 注入时钟，测试用。
func WithClock(f func() int64) ScannerOption {
	return func(s *Scanner) { s.now = f }
}

// WithResolver 换挂载点解析器。
func WithResolver(r MountResolver) ScannerOption {
	return func(s *Scanner) { s.resolver = r }
}

// NewScanner 构造扫描器。
func NewScanner(st Store, opts ...ScannerOption) *Scanner {
	s := &Scanner{
		store:    st,
		resolver: HintResolver{},
		batch:    DefaultBatchSize,
		now:      store.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// scanCandidate 是遍历时收集到的一个候选文件。
type scanCandidate struct {
	volRel  string // 相对卷根：算 path_key
	libRel  string // 相对库根：展示与去重以外的一切
	size    int64
	mtimeMS int64
}

// ScanLibrary 扫描一个库。
//
// 顺序是固定的，每一步都有理由：
//  1. 先看卷状态——卷离线时连 stat 都不要做，Windows 上访问已拔出的盘符会阻塞数秒
//  2. 再解析挂载点
//  3. 遍历、过滤、归一化、分批比对
//  4. 只有遍历完整结束，才做消失判定
func (s *Scanner) ScanLibrary(ctx context.Context, libraryID string, onProgress func(Progress)) (ScanResult, error) {
	res := ScanResult{LibraryID: libraryID}

	lib, err := s.store.Library(ctx, libraryID)
	if err != nil {
		return res, fmt.Errorf("扫描库 %s: %w", libraryID, err)
	}
	if !lib.Enabled {
		return res, nil
	}

	vol, err := s.store.Volume(ctx, lib.VolumeID)
	if err != nil {
		return res, fmt.Errorf("扫描库 %s: %w", libraryID, err)
	}

	mount, err := s.resolver.Resolve(vol)
	if err != nil {
		// 卷离线上报状态，不当作任务失败。
		_ = s.store.SetVolumeStatus(ctx, vol.ID, "offline", s.now())
		return res, err
	}

	// 卷能解析说明它现在是活的；把状态从 offline 拉回来。
	if vol.Status != "online" {
		_ = s.store.SetVolumeStatus(ctx, vol.ID, "online", s.now())
	}

	// 先把库里已有的 key 全部取出来。遍历时见一个删一个，
	// 走完还剩下的就是"库里记得、盘上没有"的。
	existing, err := s.store.ListLibraryPaths(ctx, lib.ID)
	if err != nil {
		return res, fmt.Errorf("扫描库 %s: %w", libraryID, err)
	}
	stale := make(map[string]store.PathRef, len(existing))
	for _, p := range existing {
		stale[p.PathKey] = p
	}

	start := lib.RootRel
	if start == "" {
		start = "."
	}

	buf := make([]scanCandidate, 0, s.batch)
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		if err := s.applyBatch(ctx, lib, buf, stale, &res); err != nil {
			return err
		}
		buf = buf[:0]
		if onProgress != nil {
			onProgress(Progress{Done: res.Scanned, Unit: "files"})
		}
		// 协作式取消：只在批次边界检查，避免每文件一次 ctx 查询。
		return ctx.Err()
	}

	root := os.DirFS(mount)
	walkErr := fs.WalkDir(root, start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return s.classifyWalkErr(p, start, &res, err)
		}
		name := d.Name()
		if d.IsDir() {
			if domain.IsJunk(name) {
				return fs.SkipDir
			}
			return nil
		}
		if domain.IsJunk(name) || !domain.IsVideoFile(name) {
			return nil
		}

		info, ierr := d.Info()
		if ierr != nil {
			// 单个文件 stat 失败不该中断整卷，但要让结果标记为不完整。
			res.Partial = true
			return nil
		}

		res.Scanned++
		buf = append(buf, scanCandidate{
			volRel:  p,
			libRel:  libRelPath(p, start),
			size:    info.Size(),
			mtimeMS: info.ModTime().UnixMilli(),
		})
		if len(buf) >= s.batch {
			return flush()
		}
		return nil
	})

	if walkErr != nil {
		if errors.Is(walkErr, errVolumeGone) {
			_ = s.store.SetVolumeStatus(ctx, vol.ID, "offline", s.now())
			return res, walkErr
		}
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			// 取消：已处理的部分保留，不做消失判定。
			res.Partial = true
			_ = flush()
			return res, walkErr
		}
		return res, walkErr
	}
	if err := flush(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			res.Partial = true
			return res, err
		}
		return res, err
	}

	// 只有完整遍历才敢判消失。
	if !res.Partial && len(stale) > 0 {
		refs := make([]store.PathRef, 0, len(stale))
		for _, p := range stale {
			refs = append(refs, p)
			res.MissingPaths = append(res.MissingPaths, p.RelPath)
		}
		n, err := s.store.MarkMissing(ctx, refs, s.now())
		if err != nil {
			return res, fmt.Errorf("标记消失: %w", err)
		}
		res.Removed = n
	}

	if err := s.store.TouchLibraryScan(ctx, lib.ID, s.now()); err != nil {
		return res, err
	}
	if onProgress != nil {
		onProgress(Progress{Done: res.Scanned, Unit: "files"})
	}
	return res, nil
}

// applyBatch 处理一批文件：查已有记录、决定新增/更新/跳过。
func (s *Scanner) applyBatch(ctx context.Context, lib store.Library, buf []scanCandidate, stale map[string]store.PathRef, res *ScanResult) error {
	keys := make([]string, len(buf))
	for i, f := range buf {
		keys[i] = domain.VolumePathKey(lib.VolumeID, lib.RootRel, f.libRel)
	}

	known, err := s.store.KnownFiles(ctx, keys)
	if err != nil {
		return err
	}

	var toInsert []store.UnmatchedRow
	var toClear []store.PathRef

	for i, f := range buf {
		key := keys[i]
		// 无论后面走哪条分支，这个文件都"见到了"，从消失候选里划掉。
		delete(stale, key)

		kf, ok := known[key]
		if !ok {
			toInsert = append(toInsert, store.UnmatchedRow{
				VolumeID:  lib.VolumeID,
				LibraryID: lib.ID,
				RelPath:   f.libRel,
				PathKey:   key,
				SizeBytes: f.size,
				MTime:     f.mtimeMS,
			})
			res.Added++
			continue
		}

		changed := kf.SizeBytes != f.size || kf.MTime != f.mtimeMS
		switch {
		case kf.Missing:
			// 之前被标过消失，现在又见到了。算「恢复」，不算「新增」——
			// 29 屏的「已恢复 N 项」和「新增 N 项」是两件不同的事。
			//
			// 文件可能原样回来（stat 完全一致，走不到 changed），
			// 也可能在离线期间被替换过，两种情况都要清标记。
			if changed {
				if kf.Table == "media" {
					if err := s.store.TouchMedia(ctx, kf.ID, f.size, f.mtimeMS); err != nil {
						return err
					}
				} else if err := s.store.TouchUnmatched(ctx, kf.ID, f.size, f.mtimeMS, true); err != nil {
					return err
				}
			} else {
				toClear = append(toClear, store.PathRef{Table: kf.Table, ID: kf.ID, PathKey: key})
			}
			res.Restored++
		case changed && kf.Table == "media":
			// 已入库的片子内容变了：只更新 stat，探测交给调用方在锁外做。
			if err := s.store.TouchMedia(ctx, kf.ID, f.size, f.mtimeMS); err != nil {
				return err
			}
			res.NeedsReprobe = append(res.NeedsReprobe, f.volRel)
			res.Updated++
		case changed:
			if err := s.store.TouchUnmatched(ctx, kf.ID, f.size, f.mtimeMS, true); err != nil {
				return err
			}
			res.Updated++
		default:
			res.Unchanged++
		}
	}

	if len(toInsert) > 0 {
		if _, err := s.store.InsertUnmatchedBatch(ctx, toInsert); err != nil {
			return err
		}
	}
	if len(toClear) > 0 {
		if _, err := s.store.ClearMissing(ctx, toClear); err != nil {
			return err
		}
	}
	return nil
}

// classifyWalkErr 判断遍历遇到的错误属于哪一类。
//
// 三类处理方式完全不同，混为一谈就是数据事故：
//   - 权限错误：标记不完整，继续走（文件还在，只是读不到）
//   - 卷消失：整体中止（后续所有 stat 都会失败，会把整库误判为已删除）
//   - 文件不存在：遍历途中有文件被删，跳过即可
func (s *Scanner) classifyWalkErr(p, start string, res *ScanResult, err error) error {
	if isVolumeGone(err) {
		return errVolumeGone
	}
	// 根目录都读不到，等同于卷消失——注意这里用的是"没有这个目录"判断，
	// 而不是"任意子目录读不到"：后者可能只是一个被独占的子目录。
	if p == start && errors.Is(err, fs.ErrNotExist) {
		return errVolumeGone
	}
	if errors.Is(err, fs.ErrPermission) {
		res.Partial = true
		// 目录读不进去要整枝跳过，否则 WalkDir 会反复重试同一个目录。
		return fs.SkipDir
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	res.Partial = true
	return nil
}

// libRelPath 把卷相对路径转成库相对路径。
func libRelPath(volRel, rootRel string) string {
	if rootRel == "" || rootRel == "." {
		return path.Clean(volRel)
	}
	prefix := strings.TrimSuffix(rootRel, "/") + "/"
	if s := strings.TrimPrefix(volRel, prefix); s != volRel {
		return s
	}
	return path.Clean(volRel)
}
