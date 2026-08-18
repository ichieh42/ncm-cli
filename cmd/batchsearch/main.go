// 批量搜索 YTM 独有歌曲在网易云的候选歌曲
// 用法: go run ./cmd/batchsearch -input .ytm-only.json -output .ytm-search.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"ncm-cli/internal/config"
	"ncm-cli/internal/ncm"
)

type YTMSong struct {
	Vid     string `json:"vid"`
	Title   string `json:"title"`
	Artists string `json:"artists"`
	Album   string `json:"album"`
	Dur     string `json:"dur"`
	DurSec  int    `json:"dur_sec"`
	Bucket  string `json:"bucket,omitempty"`
	Reason  string `json:"reason,omitempty"`
	TitleS  string `json:"title_s,omitempty"`
	ArtistS string `json:"artist_s,omitempty"`
}

type Candidate struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Artists     []string `json:"artists"`
	Album       string   `json:"album"`
	DT          int64    `json:"dt"`
	Score       float64  `json:"score"`
	TitleScore  float64  `json:"titleScore"`
	ArtistScore float64  `json:"artistScore"`
	DurScore    float64  `json:"durScore"`
}

type Result struct {
	Vid        string      `json:"vid"`
	Title      string      `json:"title"`
	Artists    string      `json:"artists"`
	Dur        string      `json:"dur"`
	Bucket     string      `json:"bucket"`
	Keyword    string      `json:"keyword"`
	Candidates []Candidate `json:"candidates"`
}

var (
	repl = strings.NewReplacer(
		" ", "", "-", "", "–", "", "—", "", "~", "", "～", "", "·", "", "・", "",
		",", "", "，", "", "。", "", ".", "", "．", "", "&", "", "(", "", ")", "",
		"（", "", "）", "", "[", "", "]", "", "【", "", "】", "", "《", "", "》", "",
		"'", "", "\"", "", "“", "", "”", "", "‘", "", "’", "", "/", "", "\\", "", "|", "",
	)
)

func norm(s string) string {
	s = strings.ToLower(repl.Replace(s))
	// 去掉括号内内容（如 feat. xxx、Live 标注）
	for {
		i := strings.Index(s, "feat")
		if i < 0 {
			break
		}
		// 找 feat 到下一个非字母位置
		j := i
		for j < len(s) && (s[j] >= 'a' && s[j] <= 'z' || s[j] >= '0' && s[j] <= '9') {
			j++
		}
		s = s[:i] + s[j:]
	}
	return s
}

// sim 计算归一化标题相似度 0-1
func sim(a, b string) float64 {
	na, nb := norm(a), norm(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	short, long := na, nb
	if len(short) > len(long) {
		short, long = long, short
	}
	if len(short) >= 3 && strings.Contains(long, short) {
		return 0.85
	}
	best := 0.0
	for i := 0; i+len(short) <= len(long); i++ {
		match := 0
		for j := 0; j < len(short); j++ {
			if short[j] == long[i+j] {
				match++
			}
		}
		r := float64(match) / float64(len(short))
		if r > best {
			best = r
		}
	}
	if best >= 0.7 {
		return best
	}
	return 0
}

func score(s YTMSong, cand ncm.Song) (float64, float64, float64, float64) {
	ytmTitle := s.TitleS
	if ytmTitle == "" {
		ytmTitle = s.Title
	}
	titleScore := sim(ytmTitle, cand.Name)
	// 歌手匹配（用简体）
	ytmArtistRaw := s.ArtistS
	if ytmArtistRaw == "" {
		ytmArtistRaw = s.Artists
	}
	ytmArtists := splitArtists(ytmArtistRaw)
	candArtists := make([]string, 0, len(cand.Artists))
	for _, a := range cand.Artists {
		candArtists = append(candArtists, norm(a.Name))
	}
	artistScore := 0.0
	for _, ya := range ytmArtists {
		if ya == "" {
			continue
		}
		found := false
		for _, ca := range candArtists {
			if ca == ya {
				artistScore = 1
				found = true
				break
			}
		}
		if found {
			break
		}
		for _, ca := range candArtists {
			if strings.Contains(ca, ya) || strings.Contains(ya, ca) {
				if artistScore < 0.6 {
					artistScore = 0.6
				}
			}
		}
	}
	// 时长匹配
	durScore := 0.0
	if s.DurSec > 0 && cand.Duration > 0 {
		diff := s.DurSec - int(cand.Duration/1000)
		if diff < 0 {
			diff = -diff
		}
		switch {
		case diff <= 5:
			durScore = 1
		case diff <= 15:
			durScore = 0.7
		case diff <= 30:
			durScore = 0.4
		}
	}
	total := titleScore*0.6 + artistScore*0.25 + durScore*0.15
	return total, titleScore, artistScore, durScore
}

func splitArtists(s string) []string {
	parts := strings.Split(s, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.SplitN(p, "(", 2)[0]
		p = strings.SplitN(p, "（", 2)[0]
		p = strings.SplitN(p, "feat", 2)[0]
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, norm(p))
		}
	}
	return out
}

// keyword 生成搜索词：标题主段 + 首位歌手（优先用简体）
func keyword(s YTMSong) string {
	title := s.TitleS
	if title == "" {
		title = s.Title
	}
	// 取 " - " 前的主标题（YTM 常见 "中文 - 英文" 或反之）
	if i := strings.Index(title, " - "); i > 0 {
		title = title[:i]
	}
	// 去掉括号标注
	if i := strings.Index(title, "("); i > 0 {
		title = title[:i]
	}
	if i := strings.Index(title, "（"); i > 0 {
		title = title[:i]
	}
	title = strings.TrimSpace(title)
	artistS := s.ArtistS
	if artistS == "" {
		artistS = s.Artists
	}
	artists := splitArtists(artistS)
	kw := title
	if len(artists) > 0 && artists[0] != "" {
		kw = title + " " + artists[0]
	}
	return strings.TrimSpace(kw)
}

func main() {
	var input, output string
	flag.StringVar(&input, "input", ".ytm-only.json", "输入 YTM 独有歌曲 JSON")
	flag.StringVar(&output, "output", ".ytm-search.json", "输出搜索结果 JSON")
	flag.Parse()

	dir, err := config.Resolve("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve:", err)
		os.Exit(1)
	}
	statePath, _, err := config.ExistingStorageState(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "state:", err)
		os.Exit(1)
	}
	state, err := config.LoadStorageState(statePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
	client, err := ncm.NewClientFromStorageState(state, 30*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, "client:", err)
		os.Exit(1)
	}

	data, err := os.ReadFile(input)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read input:", err)
		os.Exit(1)
	}
	var songs []YTMSong
	if err := json.Unmarshal(data, &songs); err != nil {
		fmt.Fprintln(os.Stderr, "parse input:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	results := make([]Result, 0, len(songs))
	for i, s := range songs {
		kw := keyword(s)
		var resp *ncm.SearchSongResponse
		var err error
		// 先精确搜（标题+歌手），失败退化为仅标题
		for attempt := 0; attempt < 2; attempt++ {
			query := kw
			if attempt == 1 {
				title := s.TitleS
				if title == "" {
					title = s.Title
				}
				if idx := strings.Index(title, " - "); idx > 0 {
					title = title[:idx]
				}
				query = strings.TrimSpace(strings.SplitN(title, "(", 2)[0])
			}
			resp, err = client.SearchSongs(ctx, query, 10, 0)
			if err == nil {
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		r := Result{
			Vid:     s.Vid,
			Title:   s.Title,
			Artists: s.Artists,
			Dur:     s.Dur,
			Bucket:  s.Bucket,
			Keyword: kw,
		}
		if err == nil && resp != nil {
			for _, c := range resp.Result.Songs {
				total, ts, as, ds := score(s, c)
				if total <= 0 {
					continue
				}
				r.Candidates = append(r.Candidates, Candidate{
					ID:          c.ID,
					Name:        c.Name,
					Artists:     artistNames(c.Artists),
					Album:       c.Album.Name,
					DT:          c.Duration,
					Score:       round3(total),
					TitleScore:  round3(ts),
					ArtistScore: round3(as),
					DurScore:    round3(ds),
				})
			}
			// 排序取前5
			for i := 0; i < len(r.Candidates); i++ {
				for j := i + 1; j < len(r.Candidates); j++ {
					if r.Candidates[j].Score > r.Candidates[i].Score {
						r.Candidates[i], r.Candidates[j] = r.Candidates[j], r.Candidates[i]
					}
				}
			}
			if len(r.Candidates) > 5 {
				r.Candidates = r.Candidates[:5]
			}
		} else if err != nil {
			fmt.Fprintf(os.Stderr, "[%d/%d] ERR %s: %v\n", i+1, len(songs), kw, err)
		}
		results = append(results, r)
		if (i+1)%25 == 0 || i+1 == len(songs) {
			fmt.Fprintf(os.Stderr, "progress %d/%d\n", i+1, len(songs))
		}
		time.Sleep(150 * time.Millisecond)
	}

	out, _ := json.MarshalIndent(results, "", " ")
	if err := os.WriteFile(output, out, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("done %d -> %s\n", len(results), output)
}

func artistNames(as []ncm.Artist) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

func round3(v float64) float64 {
	return float64(int(v*1000+0.5)) / 1000
}
