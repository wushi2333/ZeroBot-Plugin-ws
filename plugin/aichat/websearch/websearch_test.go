package websearch

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/FloatTech/zbputils/ctxext"
)

const bingPage = `<html><ol id="b_results">
<li class="b_algo" data-id iid=SERP.5326><div class="b_tpcn"><a class="tilk" href="https://wutheringwaves.kurogames.com/"><div class="tptt">鸣潮</div></a></div>
<h2 class=""><a href="https://www.bing.com/ck/a?!&amp;&amp;p=1" h="ID=SERP,5">《鸣潮》官方网站 - <strong>Wuthering</strong> Waves</a></h2>
<div class="b_caption"><p class="b_lineclamp2 b_algoSlug"><span class="news_dt">2026年7月1日</span>&nbsp;&#0183; 《鸣潮》是一款开放世界&ensp;动作游戏。</p></div></li>
<li class="b_algo"><h2><a href="x">鸣（汉语汉字）_百度百科</a></h2><div class="b_caption"><p>又兽亦曰鸣。</p></div></li>
<li class="b_algo"><div>no title here</div></li>
</ol></html>`

const ddgPage = `<div class="result"><h2 class="result__title"><a rel="nofollow" class="result__a" href="//duckduckgo.com/l/?uddg=x">DeepSeek &amp; V4</a></h2>
<a class="result__snippet" href="x">DeepSeek <b>V4</b> 发布了</a></div>`

func TestEnabledDefaultOn(t *testing.T) {
	var s ctxext.Storage
	if !Enabled(s) || Enabled(s.Set(1, BitmapNsrch)) {
		t.Fatal("search must be on by default and off when the bit is set")
	}
}

func TestParseBing(t *testing.T) {
	rs := parseBing(bingPage)
	if len(rs) != 2 {
		t.Fatalf("want 2 results, got %+v", rs)
	}
	if rs[0].Title != "《鸣潮》官方网站 - Wuthering Waves" || !strings.HasPrefix(rs[0].Snippet, "2026年7月1日") ||
		strings.ContainsAny(rs[0].Snippet, "<>&") {
		t.Fatalf("tags/entities must be cleaned: %+v", rs[0])
	}
	if rs[1].Snippet != "又兽亦曰鸣。" {
		t.Fatalf("plain <p> fallback: %+v", rs[1])
	}
	if len(parseBing("<html>no results</html>")) != 0 {
		t.Fatal("page without results")
	}
}

func TestParseDDG(t *testing.T) {
	rs := parseDDG(ddgPage)
	if len(rs) != 1 || rs[0].Title != "DeepSeek & V4" || rs[0].Snippet != "DeepSeek V4 发布了" {
		t.Fatalf("unexpected %+v", rs)
	}
}

func TestSearchFallbackAndFormat(t *testing.T) {
	old := engines
	defer func() { engines = old }()
	engines = []func(string) ([]Result, error){
		func(string) ([]Result, error) { return nil, errors.New("bing down") },
		func(q string) ([]Result, error) {
			return []Result{{Title: "标题", Snippet: q}, {Title: "只有标题"}}, nil
		},
	}
	rs, err := Search(" 鸣潮 ")
	if err != nil || len(rs) != 2 {
		t.Fatalf("must fall back to the next engine: %v %v", rs, err)
	}
	if Format(rs) != "1. 标题：鸣潮\n2. 只有标题" {
		t.Fatalf("unexpected format %q", Format(rs))
	}
	engines = []func(string) ([]Result, error){func(string) ([]Result, error) { return nil, nil }}
	if rs, err := Search("x"); err != nil || len(rs) != 0 {
		t.Fatal("nothing found is not an error")
	}
	if _, err := Search("  "); err == nil {
		t.Fatal("empty query must be rejected")
	}
}

const newsRSS = `<?xml version="1.0" encoding="utf-8" ?><rss version="2.0" xmlns:News="https://www.bing.com/news/search?format=rss&amp;q=DeepSeek"><channel><title>DeepSeek - 必应资讯</title>
<item><title>DeepSeek V4.1 Flash 模型正式发布</title><link>http://www.bing.com/news/apiclick.aspx?a=1&amp;b=2</link><description>快科技10月6日消息，DeepSeek &lt;b&gt;今日&lt;/b&gt;发布……</description><pubDate>Mon, 05 Oct 2026 23:43:24 GMT</pubDate><News:Source>快科技 on MSN</News:Source><News:Image>http://x</News:Image></item>
<item><title>没有日期的</title><description>d</description></item>
</channel></rss>`

func TestParseBingNews(t *testing.T) {
	rs, err := parseBingNews(newsRSS)
	if err != nil || len(rs) != 2 {
		t.Fatalf("want 2 items: %v %+v", err, rs)
	}
	// 23:43 GMT 是北京时间第二天
	if rs[0].Date != "2026年10月6日" || rs[0].Source != "快科技" || rs[0].Snippet != "快科技10月6日消息，DeepSeek 今日发布……" {
		t.Fatalf("unexpected %+v", rs[0])
	}
	if !strings.HasPrefix(Format(rs[:1]), "1. DeepSeek V4.1 Flash 模型正式发布（快科技，2026年10月6日）：") {
		t.Fatalf("date/source must be shown: %q", Format(rs[:1]))
	}
	if _, err := parseBingNews("<html>not rss"); err == nil {
		t.Fatal("non-xml must be an error")
	}
}

func TestCleanKeepsHighlightedWordsTogether(t *testing.T) {
	if got := clean(`《<strong>鸣</strong><strong>潮</strong>》官方网站<br>第二行`, 50); got != "《鸣潮》官方网站 第二行" {
		t.Fatalf("got %q", got)
	}
}

func TestKeywordsAndRelevant(t *testing.T) {
	ks := strings.Join(keywords("2026年10月 新番  鸣潮最新 DeepSeek V4 3.5 a"), ",")
	if ks != "新番,鸣潮,潮最,最新,deepseek,v4" {
		t.Fatalf("unexpected keywords %s", ks)
	}
	if ks := keywords("2026年诺贝尔文学奖"); len(ks) == 0 || ks[0] != "年诺" {
		t.Fatalf("mixed token must keep han bigrams: %v", ks)
	}
	rs := []Result{
		{Title: "Calendar Malaysia 2026", Snippet: "public holidays"},
		{Title: "鸣的意思,鸣的解释", Snippet: "〔鸣〕字仓颉码"},
		{Title: "《鸣潮》官方网站"},
		{Title: "十月新番导视", Snippet: "本季"},
		{Title: "DEEPSEEK 发布会"},
	}
	got := ""
	for _, r := range relevant("鸣潮 新番 deepseek 2026年", rs) {
		got += r.Title + "|"
	}
	if got != "《鸣潮》官方网站|十月新番导视|DEEPSEEK 发布会|" {
		t.Fatalf("unrelated results must be dropped, got %s", got)
	}
	if len(relevant("2026", rs)) != len(rs) {
		t.Fatal("no usable keywords means no filtering")
	}
}

func TestMergeNewsFirst(t *testing.T) {
	n := []Result{{Title: "n1"}, {Title: "n2"}, {Title: "n3"}, {Title: "n4"}, {Title: "w1"}}
	w := []Result{{Title: "w1"}, {Title: "w2"}, {Title: "w3"}}
	got := ""
	for _, r := range merge(n, w) {
		got += r.Title + ","
	}
	if got != "n1,n2,n3,w1,w2," {
		t.Fatalf("news first (max 3), then web, deduped: %s", got)
	}
	got = ""
	for _, r := range merge(n, nil) {
		got += r.Title + ","
	}
	if got != "n1,n2,n3,n4,w1," {
		t.Fatalf("without web, news fills up: %s", got)
	}
}

func TestAPIProviders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/bocha":
			_, _ = w.Write([]byte(`{"code":200,"msg":null,"data":{"webPages":{"value":[{"name":"鸣潮3.5版本前瞻","url":"u","snippet":"短","summary":"长摘要 ` + body["query"].(string) + `","siteName":"游民星空","datePublished":"2026-10-05T20:00:00+08:00"}]}}}`))
		case "/tavily":
			_, _ = w.Write([]byte(`{"query":"q","results":[{"title":"T","url":"u","content":"C","score":0.9}]}`))
		}
	}))
	defer srv.Close()
	ob, ot := bochaAPI, tavilyAPI
	defer func() { bochaAPI, tavilyAPI = ob, ot }()
	bochaAPI, tavilyAPI = srv.URL+"/bocha", srv.URL+"/tavily"

	t.Setenv("BOCHA_API_KEY", "")
	t.Setenv("TAVILY_API_KEY", "")
	if rs, err := bocha("x"); rs != nil || err != nil {
		t.Fatal("without key the provider must be skipped")
	}
	t.Setenv("BOCHA_API_KEY", "k")
	rs, err := bocha("鸣潮")
	if err != nil || len(rs) != 1 || rs[0].Snippet != "长摘要 鸣潮" || rs[0].Source != "游民星空" || rs[0].Date != "2026年10月5日" {
		t.Fatalf("bocha: %v %+v", err, rs)
	}
	t.Setenv("TAVILY_API_KEY", "k")
	rs, err = tavily("q")
	if err != nil || len(rs) != 1 || rs[0].Title != "T" || rs[0].Snippet != "C" {
		t.Fatalf("tavily: %v %+v", err, rs)
	}
	t.Setenv("TAVILY_API_KEY", "bad")
	if _, err := tavily("q"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("http errors must surface so the next engine is tried: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	limu.Lock()
	recent = nil
	limu.Unlock()
	now := time.Now()
	for i := 0; i < hourLimit; i++ {
		if !allow(now) {
			t.Fatalf("request %d must be allowed", i)
		}
	}
	if allow(now) {
		t.Fatal("over the hourly limit must be refused")
	}
	if !allow(now.Add(time.Hour + time.Second)) {
		t.Fatal("window must slide")
	}
}
