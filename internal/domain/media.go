package domain

import (
	"path"
	"strings"
)

// VolumePathKey 用**卷相对路径**算 path_key。
//
// 为什么不是库相对路径：框架 §3.1 把 `rel_path` 定义为「相对库根」，
// 但 path_key 又用它算。同一卷上 `media/movie/a.mkv` 与 `media/tv/a.mkv`
// 的库相对路径都是 `a.mkv`，键会撞，第二个文件被唯一约束挡掉。
//
// 所以两个东西分工：
//
//	rel_path  = 相对库根（展示用，客户端拼 {libraryName}/{relPath}，见契约 241 行）
//	path_key  = 相对卷根（去重用，rootRel + libRel）
//
// 这和 §3.1 的字面写法有一处偏差，已在框架 v3.6 记明。
func VolumePathKey(volumeID, rootRel, libRel string) string {
	volRel := libRel
	if r, ok := CleanRel(rootRel); ok && r != "" {
		volRel = r + "/" + strings.TrimPrefix(libRel, "/")
	}
	return PathKey(volumeID, volRel)
}

// 视频扩展名。一期只做电影与剧集，音乐/照片的扩展名先不收。
//
// 不含 .iso：光盘镜像不是"文件"，取帧、remux、进度都走不通，
// 收进来只会在 28 屏堆出一堆永远处理不掉的条目。
var videoExts = map[string]struct{}{
	".mkv":  {},
	".mp4":  {},
	".m4v":  {},
	".mov":  {},
	".avi":  {},
	".wmv":  {},
	".flv":  {},
	".webm": {},
	".ts":   {},
	".m2ts": {},
	".mts":  {},
	".mpg":  {},
	".mpeg": {},
	".ogv":  {},
	".3gp":  {},
	".rmvb": {},
	".rm":   {},
}

// IsVideoFile 判断文件名是否为视频。
func IsVideoFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	_, ok := videoExts[ext]
	return ok
}

// IsVideoExt 判断扩展名（含点）是否受支持。
func IsVideoExt(ext string) bool {
	_, ok := videoExts[strings.ToLower(ext)]
	return ok
}
