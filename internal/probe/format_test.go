package probe

import "testing"

// 真实 mkv 的 ffprobe -show_format 形状（节选）。
func TestParseFormatTags(t *testing.T) {
	raw := []byte(`{
	  "format": {
	    "filename": "All.Wishes.Come.True.2026.2160p.WEB-DL.mkv",
	    "nb_streams": 3,
	    "format_name": "matroska,webm",
	    "tags": {
	      "encoder": "libebml v1.4.4 + libmatroska v1.7.1",
	      "TMDB": "movie/1633056",
	      "IMDB": "tt43515101"
	    }
	  }
	}`)
	tags, err := ParseFormatTags(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tags["TMDB"] != "movie/1633056" {
		t.Errorf("TMDB = %q", tags["TMDB"])
	}
	if tags["IMDB"] != "tt43515101" {
		t.Errorf("IMDB = %q", tags["IMDB"])
	}
}

// 有些封装把 tag 写成数字。若直接解进 map[string]string，整条会失败，
// 于是"这个文件没有 TMDB tag"——而它其实有。
func TestParseFormatTagsAcceptsNonString(t *testing.T) {
	raw := []byte(`{"format":{"tags":{"TMDB":1633056,"track":3}}}`)
	tags, err := ParseFormatTags(raw)
	if err != nil {
		t.Fatal(err)
	}
	if tags["TMDB"] != "1633056" {
		t.Errorf("数字 tag 没转成字符串：%q", tags["TMDB"])
	}
}

func TestParseFormatTagsWithoutTags(t *testing.T) {
	tags, err := ParseFormatTags([]byte(`{"format":{"format_name":"mp4"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 0 {
		t.Errorf("无 tags 时应返回空 map，实际 %v", tags)
	}
}
