// Package websearch AI 聊天的联网搜索
//
// 依次尝试:
//  1. 环境变量里配置了 key 的搜索 API: BOCHA_API_KEY (博查, 中文结果好), TAVILY_API_KEY
//  2. 参考 AstrBot 等开源 bot 的默认做法, 不需要 key: 同时抓取 Bing 资讯 RSS (新闻、版本更新这类
//     时效性内容准确得多) 和 Bing 网页结果, 资讯在前合并
//  3. DuckDuckGo
//
// 只取前几条标题和摘要给模型参考, 不打开网页正文.
package websearch

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/FloatTech/zbputils/ctxext"
)

// BitmapNsrch 置 1 表示本群不使用联网搜索 (aichatcfg 存储位, 默认使用)
const BitmapNsrch = 0x1000000

// Enabled 本群是否使用联网搜索
func Enabled(stor ctxext.Storage) bool {
	return !stor.GetBool(BitmapNsrch)
}

const (
	maxResults = 5
	titleLen   = 60
	snippetLen = 160
	pageBytes  = 2 << 20
	hourLimit  = 60 // 全局每小时最多搜索次数, 防止被刷或被搜索引擎封
)

const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

// ErrRateLimited 搜索太频繁
var ErrRateLimited = errors.New("搜索太频繁")

// Result 一条搜索结果
type Result struct {
	Title   string
	Snippet string
	Source  string // 来源, 可为空
	Date    string // 发布日期 (北京时间 "1月2日"), 可为空
}

var cst = time.FixedZone("CST", 8*3600)

func shortdate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(cst).Format("2006年1月2日")
}

var client = &http.Client{Timeout: 10 * time.Second}

var (
	limu   sync.Mutex
	recent []time.Time
)

// allow 滑动窗口限流
func allow(now time.Time) bool {
	limu.Lock()
	defer limu.Unlock()
	i := 0
	for i < len(recent) && now.Sub(recent[i]) > time.Hour {
		i++
	}
	recent = recent[i:]
	if len(recent) >= hourLimit {
		return false
	}
	recent = append(recent, now)
	return true
}

// engines 依次尝试的搜索引擎, 测试时可替换. 没配置 key 的 API 返回 (nil, nil) 直接跳过.
var engines = []func(q string) ([]Result, error){bocha, tavily, bing, ddg}

// Search 搜索 q, 返回前几条结果的标题和摘要. 都没搜到时返回空切片.
func Search(q string) ([]Result, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, errors.New("empty query")
	}
	if !allow(time.Now()) {
		return nil, ErrRateLimited
	}
	errs := make([]error, 0, len(engines))
	for _, e := range engines {
		rs, err := e(q)
		if len(rs) > 0 {
			return rs, nil
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return nil, errors.Join(errs...)
}

// Format 把结果排成给模型看的几行文字
func Format(rs []Result) string {
	sb := strings.Builder{}
	for i, r := range rs {
		sb.WriteString(strconv.Itoa(i + 1))
		sb.WriteString(". ")
		sb.WriteString(r.Title)
		if meta := strings.Trim(r.Source+"，"+r.Date, "，"); meta != "" {
			sb.WriteString("（")
			sb.WriteString(meta)
			sb.WriteString("）")
		}
		if r.Snippet != "" {
			sb.WriteString("：")
			sb.WriteString(r.Snippet)
		}
		sb.WriteByte('\n')
	}
	return strings.TrimSpace(sb.String())
}

func get(u string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, pageBytes))
	return string(b), err
}

var (
	tagre    = regexp.MustCompile(`<[^>]*>`)
	inlinere = regexp.MustCompile(`(?i)</?(strong|b|em|i|mark|span|font)\b[^>]*>`)
	h2re     = regexp.MustCompile(`(?s)<h2[^>]*>(.*?)</h2>`)
	clampre  = regexp.MustCompile(`(?s)<p[^>]*class="[^"]*b_lineclamp[^"]*"[^>]*>(.*?)</p>`)
	pre      = regexp.MustCompile(`(?s)<p[^>]*>(.*?)</p>`)
	ddgre    = regexp.MustCompile(`(?s)class="result__a"[^>]*>(.*?)</a>.*?class="result__snippet"[^>]*>(.*?)</a>`)
	datere   = regexp.MustCompile(`^[0-9]+(年|月|日|号)?([0-9]+(月|日|号))*$`)
)

// clean 去掉标签和实体. 行内标签 (Bing 用 <strong> 高亮关键词) 直接删掉, 不然"鸣潮"会变成"鸣 潮"
func clean(s string, n int) string {
	s = tagre.ReplaceAllString(inlinere.ReplaceAllString(s, ""), " ")
	s = strings.Join(strings.Fields(html.UnescapeString(s)), " ")
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n]) + "…"
	}
	return s
}

// keywords 判断相关性用的词: 中文按相邻两字切, 其他按整词; 忽略纯数字、年月日和单个字符
func keywords(q string) []string {
	ks := make([]string, 0, 8)
	for _, f := range strings.Fields(strings.ToLower(q)) {
		if datere.MatchString(f) {
			continue
		}
		han, other := make([]rune, 0, len(f)), make([]rune, 0, len(f))
		flush := func() {
			for i := 0; i+1 < len(han); i++ {
				ks = append(ks, string(han[i:i+2]))
			}
			if len(other) >= 2 && strings.ContainsFunc(string(other), unicode.IsLetter) {
				ks = append(ks, string(other))
			}
			han, other = han[:0], other[:0]
		}
		for _, r := range f {
			isHan := unicode.Is(unicode.Han, r)
			if (isHan && len(other) > 0) || (!isHan && len(han) > 0) {
				flush()
			}
			if isHan {
				han = append(han, r)
			} else {
				other = append(other, r)
			}
		}
		flush()
	}
	return ks
}

// relevant 去掉和搜索词一个关键词都不沾的结果 (抓取的结果里常混着字典页、外国节假日表之类)
func relevant(q string, rs []Result) []Result {
	ks := keywords(q)
	if len(ks) == 0 {
		return rs
	}
	out := rs[:0:0]
	for _, r := range rs {
		text := strings.ToLower(r.Title + " " + r.Snippet)
		for _, k := range ks {
			if strings.Contains(text, k) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// bing 同时查资讯和网页, 资讯最多占 3 条排在前面, 剩下用网页补齐.
// 不带 mkt/cc 地区参数 (实测会间歇性返回空结果页).
func bing(q string) ([]Result, error) {
	var (
		wg              sync.WaitGroup
		news, web       []Result
		errnews, errweb error
	)
	// 资讯本来就按时间排, 搜索词里的"2026年10月"之类反而会让资讯搜不到东西
	nq := make([]string, 0, 4)
	for _, f := range strings.Fields(q) {
		if !datere.MatchString(f) {
			nq = append(nq, f)
		}
	}
	if len(nq) == 0 {
		nq = append(nq, q)
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		var page string
		if page, errnews = get("https://www.bing.com/news/search?format=rss&q=" + url.QueryEscape(strings.Join(nq, " "))); errnews == nil {
			news, errnews = parseBingNews(page)
		}
	}()
	go func() {
		defer wg.Done()
		var page string
		if page, errweb = get("https://www.bing.com/search?q=" + url.QueryEscape(q)); errweb == nil {
			web = parseBing(page)
		}
	}()
	wg.Wait()
	out := merge(relevant(q, news), relevant(q, web))
	if len(out) == 0 {
		return nil, errors.Join(errnews, errweb)
	}
	return out, nil
}

// merge 资讯最多取 3 条在前, 网页补齐, 还不够再用剩下的资讯, 按标题去重
func merge(news, web []Result) []Result {
	out := make([]Result, 0, maxResults)
	seen := map[string]struct{}{}
	push := func(rs []Result, limit int) {
		for _, r := range rs {
			if len(out) >= limit {
				return
			}
			if _, ok := seen[r.Title]; ok {
				continue
			}
			seen[r.Title] = struct{}{}
			out = append(out, r)
		}
	}
	push(news, 3)
	push(web, maxResults)
	push(news, maxResults)
	return out
}

type rssItem struct {
	Title       string `xml:"title"`
	Description string `xml:"description"`
	PubDate     string `xml:"pubDate"`
	// News:Source 等扩展字段, 其命名空间随查询词变化, 只能按本地名取
	Extra []struct {
		XMLName xml.Name
		Value   string `xml:",chardata"`
	} `xml:",any"`
}

func parseBingNews(page string) ([]Result, error) {
	var rss struct {
		Items []rssItem `xml:"channel>item"`
	}
	if err := xml.Unmarshal([]byte(page), &rss); err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(rss.Items))
	for _, it := range rss.Items {
		r := Result{Title: clean(it.Title, titleLen), Snippet: clean(it.Description, snippetLen)}
		if t, err := time.Parse(time.RFC1123, it.PubDate); err == nil {
			r.Date = shortdate(t)
		}
		for _, x := range it.Extra {
			if x.XMLName.Local == "Source" {
				r.Source = strings.TrimSuffix(clean(x.Value, 20), " on MSN")
			}
		}
		if r.Title != "" {
			out = append(out, r)
		}
	}
	return out, nil
}

func parseBing(page string) []Result {
	blocks := strings.Split(page, `class="b_algo"`)
	out := make([]Result, 0, maxResults)
	for _, b := range blocks[1:] {
		if len(b) > 16000 {
			b = b[:16000]
		}
		m := h2re.FindStringSubmatch(b)
		if m == nil {
			continue
		}
		r := Result{Title: clean(m[1], titleLen)}
		if p := clampre.FindStringSubmatch(b); p != nil {
			r.Snippet = clean(p[1], snippetLen)
		} else if p := pre.FindStringSubmatch(b); p != nil {
			r.Snippet = clean(p[1], snippetLen)
		}
		if r.Title == "" {
			continue
		}
		out = append(out, r) // 先全取, 过滤掉无关的之后再截断
	}
	return out
}

// 搜索 API 地址, 测试时可替换
var (
	bochaAPI  = "https://api.bochaai.com/v1/web-search"
	tavilyAPI = "https://api.tavily.com/search"
)

// postJSON 调用搜索 API
func postJSON(api, key string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, api, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, pageBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return errors.New(resp.Status + " " + clean(string(data), 200))
	}
	return json.Unmarshal(data, out)
}

func parsedate(s string) string {
	for _, layout := range []string{time.RFC3339, time.RFC1123, time.RFC1123Z, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return shortdate(t)
		}
	}
	return ""
}

// bocha 博查 Web Search API (BOCHA_API_KEY), 没配置 key 时跳过
func bocha(q string) ([]Result, error) {
	key := os.Getenv("BOCHA_API_KEY")
	if key == "" {
		return nil, nil
	}
	var rsp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			WebPages struct {
				Value []struct {
					Name          string `json:"name"`
					Snippet       string `json:"snippet"`
					Summary       string `json:"summary"`
					SiteName      string `json:"siteName"`
					DatePublished string `json:"datePublished"`
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
	}
	err := postJSON(bochaAPI, key,
		map[string]any{"query": q, "summary": true, "count": maxResults, "freshness": "noLimit"}, &rsp)
	if err != nil {
		return nil, err
	}
	if rsp.Code != 0 && rsp.Code != http.StatusOK {
		return nil, errors.New("bocha: " + strconv.Itoa(rsp.Code) + " " + rsp.Msg)
	}
	out := make([]Result, 0, maxResults)
	for _, v := range rsp.Data.WebPages.Value {
		sn := v.Summary
		if sn == "" {
			sn = v.Snippet
		}
		if t := clean(v.Name, titleLen); t != "" && len(out) < maxResults {
			out = append(out, Result{Title: t, Snippet: clean(sn, snippetLen), Source: clean(v.SiteName, 20), Date: parsedate(v.DatePublished)})
		}
	}
	return out, nil
}

// tavily Tavily Search API (TAVILY_API_KEY), 没配置 key 时跳过
func tavily(q string) ([]Result, error) {
	key := os.Getenv("TAVILY_API_KEY")
	if key == "" {
		return nil, nil
	}
	var rsp struct {
		Results []struct {
			Title         string `json:"title"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	err := postJSON(tavilyAPI, key,
		map[string]any{"query": q, "max_results": maxResults, "topic": "general"}, &rsp)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(rsp.Results))
	for _, v := range rsp.Results {
		if t := clean(v.Title, titleLen); t != "" {
			out = append(out, Result{Title: t, Snippet: clean(v.Content, snippetLen), Date: parsedate(v.PublishedDate)})
		}
	}
	return out, nil
}

func ddg(q string) ([]Result, error) {
	page, err := get("https://html.duckduckgo.com/html/?q=" + url.QueryEscape(q))
	if err != nil {
		return nil, err
	}
	rs := relevant(q, parseDDG(page))
	if len(rs) > maxResults {
		rs = rs[:maxResults]
	}
	return rs, nil
}

func parseDDG(page string) []Result {
	ms := ddgre.FindAllStringSubmatch(page, -1)
	out := make([]Result, 0, len(ms))
	for _, m := range ms {
		if t := clean(m[1], titleLen); t != "" {
			out = append(out, Result{Title: t, Snippet: clean(m[2], snippetLen)})
		}
	}
	return out
}
