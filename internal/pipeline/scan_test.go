package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/Chenglp/lumen/internal/domain"
	"github.com/Chenglp/lumen/internal/store"
)

// ─────────────────────────────────────────────────────────────
// 假 store：内存版，只实现扫描器用到的窄接口。
// 它同时记录 insert 批次的形状，用来验证「每 N 个文件一个事务」。
// ─────────────────────────────────────────────────────────────

type fakeRow struct {
	table   string
	id      string
	libID   string
	relPath string
	pathKey string
	size    int64
	mtime   int64
	missing int64
}

type fakeStore struct {
	mu      sync.Mutex
	libs    map[string]store.Library
	vols    map[string]store.Volume
	rows    map[string]*fakeRow // path_key -> row
	batches []int               // 每次 InsertUnmatchedBatch 的行数
	seq     int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		libs: map[string]store.Library{},
		vols: map[string]store.Volume{},
		rows: map[string]*fakeRow{},
	}
}

func (f *fakeStore) addVolume(v store.Volume) { f.vols[v.ID] = v }

func (f *fakeStore) addLibrary(l store.Library) { f.libs[l.ID] = l }

func (f *fakeStore) Library(_ context.Context, id string) (store.Library, error) {
	l, ok := f.libs[id]
	if !ok {
		return store.Library{}, store.ErrNotFound
	}
	return l, nil
}

func (f *fakeStore) Volume(_ context.Context, id string) (store.Volume, error) {
	v, ok := f.vols[id]
	if !ok {
		return store.Volume{}, store.ErrNotFound
	}
	return v, nil
}

func (f *fakeStore) KnownFiles(_ context.Context, keys []string) (map[string]store.KnownFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]store.KnownFile, len(keys))
	for _, k := range keys {
		if r, ok := f.rows[k]; ok {
			out[k] = store.KnownFile{
				Table: r.table, ID: r.id,
				SizeBytes: r.size, MTime: r.mtime, Missing: r.missing != 0,
			}
		}
	}
	return out, nil
}

func (f *fakeStore) InsertUnmatchedBatch(_ context.Context, rows []store.UnmatchedRow) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches = append(f.batches, len(rows))
	n := 0
	for _, r := range rows {
		if _, ok := f.rows[r.PathKey]; ok {
			continue
		}
		f.seq++
		f.rows[r.PathKey] = &fakeRow{
			table: "unmatched", id: "um_" + string(rune('a'+f.seq%26)) + r.PathKey[:4],
			libID: r.LibraryID, relPath: r.RelPath, pathKey: r.PathKey,
			size: r.SizeBytes, mtime: r.MTime,
		}
		n++
	}
	return n, nil
}

func (f *fakeStore) TouchUnmatched(_ context.Context, id string, size, mtime int64, reset bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.rows {
		if r.id == id {
			r.size, r.mtime, r.missing = size, mtime, 0
			return nil
		}
	}
	return nil
}

func (f *fakeStore) TouchMedia(_ context.Context, id string, size, mtime int64) error {
	return f.TouchUnmatched(context.Background(), id, size, mtime, false)
}

func (f *fakeStore) ListLibraryPaths(_ context.Context, libraryID string) ([]store.PathRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.PathRef
	for _, r := range f.rows {
		if r.libID != libraryID {
			continue
		}
		out = append(out, store.PathRef{Table: r.table, ID: r.id, PathKey: r.pathKey, RelPath: r.relPath})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PathKey < out[j].PathKey })
	return out, nil
}

func (f *fakeStore) MarkMissing(_ context.Context, refs []store.PathRef, at int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ref := range refs {
		for _, r := range f.rows {
			if r.id == ref.ID && r.missing == 0 {
				r.missing = at
				n++
			}
		}
	}
	return n, nil
}

func (f *fakeStore) ClearMissing(_ context.Context, refs []store.PathRef) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, ref := range refs {
		for _, r := range f.rows {
			if r.id == ref.ID && r.missing != 0 {
				r.missing = 0
				n++
			}
		}
	}
	return n, nil
}

func (f *fakeStore) TouchLibraryScan(_ context.Context, id string, at int64) error {
	l := f.libs[id]
	l.LastScanAt = at
	f.libs[id] = l
	return nil
}

func (f *fakeStore) SetVolumeStatus(_ context.Context, id, status string, _ int64) error {
	v := f.vols[id]
	v.Status = status
	f.vols[id] = v
	return nil
}

// ─────────────────────────────────────────────────────────────
// 测试脚手架
// ─────────────────────────────────────────────────────────────

type fixture struct {
	root  string // 卷挂载点
	fs    *fakeStore
	sc    *Scanner
	libID string
}

// newFixture 建一个临时"卷"，库根是 media/movie。
func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()

	f := newFakeStore()
	f.addVolume(store.Volume{
		ID: "vol_1", VolumeKey: "vk1", Kind: "local",
		MountHint: root, WatchMode: "native", Status: "online",
	})
	f.addLibrary(store.Library{
		ID: "lib_1", VolumeID: "vol_1", RootRel: "media/movie",
		Kind: "movie", Name: "电影", Enabled: true,
	})

	return &fixture{
		root:  root,
		fs:    f,
		sc:    NewScanner(f),
		libID: "lib_1",
	}
}

// write 在库根下写一个文件，并固定 mtime。
func (fx *fixture) write(t *testing.T, libRel string, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(fx.root, "media", "movie", filepath.FromSlash(libRel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件: %v", err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatalf("设 mtime: %v", err)
	}
}

func (fx *fixture) scan(t *testing.T) ScanResult {
	t.Helper()
	res, err := fx.sc.ScanLibrary(context.Background(), fx.libID, nil)
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	return res
}

var t0 = time.Date(2026, 11, 11, 0, 0, 0, 0, time.Local)

// ─────────────────────────────────────────────────────────────
// 用例
// ─────────────────────────────────────────────────────────────

func TestScanAddsNewFiles(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "千与千寻.mkv", "a", t0)
	fx.write(t, "Show/S01E01.mp4", "b", t0)

	res := fx.scan(t)
	if res.Scanned != 2 || res.Added != 2 {
		t.Fatalf("期望 scanned=2 added=2，实际 scanned=%d added=%d", res.Scanned, res.Added)
	}
	if len(fx.fs.rows) != 2 {
		t.Fatalf("库里应有 2 条记录，实际 %d", len(fx.fs.rows))
	}
}

func TestScanIsIncremental(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "a.mkv", "a", t0)
	fx.write(t, "b.mkv", "b", t0)

	fx.scan(t)
	fx.fs.batches = nil

	res := fx.scan(t)
	if res.Added != 0 || res.Unchanged != 2 {
		t.Fatalf("二次扫描应 added=0 unchanged=2，实际 added=%d unchanged=%d", res.Added, res.Unchanged)
	}
	if len(fx.fs.batches) != 0 {
		t.Fatalf("二次扫描不该再插入，实际插入了 %v", fx.fs.batches)
	}
}

func TestScanDetectsContentChange(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "a.mkv", "small", t0)
	fx.scan(t)

	// 内容变了（size 变），mtime 也变。
	fx.write(t, "a.mkv", "much bigger content", t0.Add(time.Hour))

	res := fx.scan(t)
	if res.Updated != 1 {
		t.Fatalf("应识别出 1 个变更，实际 updated=%d", res.Updated)
	}
	if res.Added != 0 {
		t.Fatalf("变更不该产生新记录，实际 added=%d", res.Added)
	}
	if len(fx.fs.rows) != 1 {
		t.Fatalf("记录数应仍为 1，实际 %d", len(fx.fs.rows))
	}
}

func TestScanSkipsJunkAndNonVideo(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "real.mkv", "a", t0)
	fx.write(t, "poster.jpg", "img", t0)
	fx.write(t, "notes.txt", "text", t0)
	fx.write(t, ".DS_Store", "junk", t0)
	fx.write(t, "@eaDir/movie.mkv", "thumb", t0)
	fx.write(t, "sample/S01E01.mkv", "real", t0)

	res := fx.scan(t)
	// 只有 real.mkv 与 sample/S01E01.mkv 该留下。
	if res.Scanned != 2 {
		t.Fatalf("期望 scanned=2，实际 %d", res.Scanned)
	}
	if _, ok := fx.fs.rows[pathKeyOf("real.mkv")]; !ok {
		t.Fatal("real.mkv 未被收录")
	}
	for k, r := range fx.fs.rows {
		if r.relPath == "poster.jpg" || r.relPath == "notes.txt" || r.relPath == ".DS_Store" {
			t.Fatalf("不该收录 %q（key=%s）", r.relPath, k)
		}
	}
}

func TestScanMarksMissingWhenComplete(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "a.mkv", "a", t0)
	fx.write(t, "b.mkv", "b", t0)
	fx.scan(t)

	if err := os.Remove(filepath.Join(fx.root, "media", "movie", "a.mkv")); err != nil {
		t.Fatal(err)
	}

	res := fx.scan(t)
	if res.Partial {
		t.Fatal("完整遍历不该标记为 incomplete")
	}
	if res.Removed != 1 {
		t.Fatalf("应标 1 条消失，实际 removed=%d", res.Removed)
	}
	if len(res.MissingPaths) != 1 || res.MissingPaths[0] != "a.mkv" {
		t.Fatalf("MissingPaths 错: %v", res.MissingPaths)
	}
	// 记录必须还在（软删），因为 playback_progress 挂着外键。
	if len(fx.fs.rows) != 2 {
		t.Fatalf("消失的文件不该被物理删除，记录数应仍为 2，实际 %d", len(fx.fs.rows))
	}
}

func TestScanRestoresReturnedFile(t *testing.T) {
	fx := newFixture(t)
	fx.write(t, "a.mkv", "a", t0)
	fx.scan(t)

	// 拔盘 → 文件消失 → 标 missing
	if err := os.Remove(filepath.Join(fx.root, "media", "movie", "a.mkv")); err != nil {
		t.Fatal(err)
	}
	fx.scan(t)

	// 插回来：内容与 mtime 完全一致（stat 不变，走不到 changed 分支）
	fx.write(t, "a.mkv", "a", t0)
	res := fx.scan(t)

	if res.Restored != 1 {
		t.Fatalf("stat 未变但文件回来了，应 restored=1，实际 %d", res.Restored)
	}
	if res.Removed != 0 {
		t.Fatalf("不该再标消失，实际 removed=%d", res.Removed)
	}
}

func TestScanOfflineVolume(t *testing.T) {
	fx := newFixture(t)
	v := fx.fs.vols["vol_1"]
	v.Status = "offline"
	fx.fs.vols["vol_1"] = v

	_, err := fx.sc.ScanLibrary(context.Background(), fx.libID, nil)
	if !errors.Is(err, ErrVolumeOffline) {
		t.Fatalf("期望 ErrVolumeOffline，实际 %v", err)
	}
	if len(fx.fs.rows) != 0 {
		t.Fatal("卷离线时不该写入任何记录")
	}
}

func TestScanMissingMountPoint(t *testing.T) {
	fx := newFixture(t)
	v := fx.fs.vols["vol_1"]
	v.MountHint = filepath.Join(fx.root, "不存在的挂载点")
	fx.fs.vols["vol_1"] = v

	_, err := fx.sc.ScanLibrary(context.Background(), fx.libID, nil)
	if !errors.Is(err, ErrVolumeOffline) {
		t.Fatalf("挂载点不存在应算卷离线，实际 %v", err)
	}
	if fx.fs.vols["vol_1"].Status != "offline" {
		t.Fatal("卷状态应被落库为 offline，否则每次扫描都会重试一次阻塞的 stat")
	}
}

func TestScanBatchesEveryN(t *testing.T) {
	fx := newFixture(t)
	fx.sc = NewScanner(fx.fs, WithBatchSize(3))
	for i := 0; i < 7; i++ {
		fx.write(t, string(rune('a'+i))+".mkv", "x", t0)
	}

	res := fx.scan(t)
	if res.Added != 7 {
		t.Fatalf("应新增 7，实际 %d", res.Added)
	}
	want := []int{3, 3, 1}
	if len(fx.fs.batches) != len(want) {
		t.Fatalf("批次形状应为 %v，实际 %v", want, fx.fs.batches)
	}
	for i := range want {
		if fx.fs.batches[i] != want[i] {
			t.Fatalf("批次形状应为 %v，实际 %v", want, fx.fs.batches)
		}
	}
}

// 同一卷上两个库可以有同名文件。库相对路径相同，但卷相对路径不同——
// path_key 必须用卷相对路径算，否则第二个文件会被唯一约束挡掉。
func TestPathKeyIsVolumeRelative(t *testing.T) {
	fx := newFixture(t)
	fx.fs.addLibrary(store.Library{
		ID: "lib_2", VolumeID: "vol_1", RootRel: "media/tv",
		Kind: "tv", Name: "剧集", Enabled: true,
	})

	fx.write(t, "a.mkv", "movie", t0) // media/movie/a.mkv
	tvDir := filepath.Join(fx.root, "media", "tv")
	if err := os.MkdirAll(tvDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tvDir, "a.mkv"), []byte("tv"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(tvDir, "a.mkv"), t0, t0); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.sc.ScanLibrary(context.Background(), "lib_1", nil); err != nil {
		t.Fatal(err)
	}
	res2, err := fx.sc.ScanLibrary(context.Background(), "lib_2", nil)
	if err != nil {
		t.Fatal(err)
	}

	if res2.Added != 1 {
		t.Fatalf("第二个库的同名文件应能入库，实际 added=%d", res2.Added)
	}
	if len(fx.fs.rows) != 2 {
		t.Fatalf("两个库应各有 1 条，实际共 %d 条（path_key 撞了）", len(fx.fs.rows))
	}
	for _, r := range fx.fs.rows {
		if r.relPath != "a.mkv" {
			t.Fatalf("库相对路径应为 a.mkv，实际 %q", r.relPath)
		}
	}
}

func TestLibRelPath(t *testing.T) {
	cases := []struct{ volRel, rootRel, want string }{
		{"media/movie/a.mkv", "media/movie", "a.mkv"},
		{"media/movie/sub/a.mkv", "media/movie", "sub/a.mkv"},
		{"a.mkv", "", "a.mkv"},
		{"a.mkv", ".", "a.mkv"},
		// 前缀相近但不能误伤：rootRel=media/mov 不该匹配 media/movie/
		{"media/movie/a.mkv", "media/mov", "media/movie/a.mkv"},
	}
	for _, c := range cases {
		if got := libRelPath(c.volRel, c.rootRel); got != c.want {
			t.Errorf("libRelPath(%q,%q) = %q, want %q", c.volRel, c.rootRel, got, c.want)
		}
	}
}

func TestClassifyWalkErr(t *testing.T) {
	var res ScanResult

	// 权限错误：标记不完整、跳过该目录，但不中止。
	if err := (&Scanner{}).classifyWalkErr("media/movie/locked", "media/movie", &res, fs.ErrPermission); !errors.Is(err, fs.SkipDir) {
		t.Fatalf("权限错误应返回 SkipDir，实际 %v", err)
	}
	if !res.Partial {
		t.Fatal("权限错误必须让结果标记为 incomplete，否则会误判消失")
	}

	// 根目录不存在：等同于卷消失。
	res = ScanResult{}
	if err := (&Scanner{}).classifyWalkErr("media/movie", "media/movie", &res, fs.ErrNotExist); !errors.Is(err, errVolumeGone) {
		t.Fatalf("根目录不存在应中止整卷，实际 %v", err)
	}

	// 子文件不存在：遍历途中有文件被删，跳过即可，不算不完整。
	res = ScanResult{}
	if err := (&Scanner{}).classifyWalkErr("media/movie/a.mkv", "media/movie", &res, fs.ErrNotExist); err != nil {
		t.Fatalf("子文件消失应静默跳过，实际 %v", err)
	}
	if res.Partial {
		t.Fatal("单个文件消失不该标记为 incomplete")
	}
}

func pathKeyOf(libRel string) string {
	// 与 fixture 的库配置一致：vol_1 + rootRel=media/movie
	return domain.VolumePathKey("vol_1", "media/movie", libRel)
}

// BenchmarkScan 用真实文件测吞吐：一次全量扫 2000 个文件。
//
// 这个数不是为了炫性能，是为了知道「首扫 27 小时」那个结论对应的
// 单库吞吐大概在什么量级——如果本机连 1000 文件/秒 都到不了，
// 那瓶颈就已经不是磁盘而是代码了。
func BenchmarkScan(b *testing.B) {
	const files = 2000
	root := b.TempDir()
	libRoot := filepath.Join(root, "media", "movie")
	for i := 0; i < files; i++ {
		dir := filepath.Join(libRoot, "show", fmt.Sprintf("s%02d", i%26))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		name := filepath.Join(dir, fmt.Sprintf("e%04d.mkv", i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}

	f := newFakeStore()
	f.addVolume(store.Volume{ID: "vol_1", VolumeKey: "vk1", Kind: "local",
		MountHint: root, WatchMode: "native", Status: "online"})
	f.addLibrary(store.Library{ID: "lib_1", VolumeID: "vol_1", RootRel: "media/movie",
		Kind: "movie", Name: "电影", Enabled: true})

	sc := NewScanner(f)
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 每轮清空记录，测的是「全量首扫」而不是「二次跳过」。
		f.mu.Lock()
		f.rows = map[string]*fakeRow{}
		f.batches = nil
		f.mu.Unlock()

		if _, err := sc.ScanLibrary(ctx, "lib_1", nil); err != nil {
			b.Fatal(err)
		}
	}
}
