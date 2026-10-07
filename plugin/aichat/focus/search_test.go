package focus

import (
	"strings"
	"testing"
	"time"
)

func TestNowLine(t *testing.T) {
	// 2026-10-08 23:59 北京时间 = 15:59 UTC, 与服务器时区无关
	now := time.Date(2026, 10, 8, 15, 59, 0, 0, time.UTC)
	if got := nowLine(now); got != "【现在】2026年10月8日 星期四 深夜 23:59" {
		t.Fatalf("got %q", got)
	}
	if got := nowLine(time.Date(2026, 10, 8, 19, 30, 0, 0, time.UTC)); got != "【现在】2026年10月9日 星期五 凌晨 03:30" {
		t.Fatalf("date must roll over in Beijing time, got %q", got)
	}
	r := &Request{sender: "丙(3)", text: "现在几点", isatme: true, now: now}
	b := r.finalUser()
	if i, j := strings.Index(b, "【现在】"), strings.Index(b, "【当前消息】"); i < 0 || i > j {
		t.Fatalf("time must be given right before the current message:\n%s", b)
	}
}

func TestSearchFlow(t *testing.T) {
	r := &Request{persona: "人设", sender: "丙(3)", text: "鸣潮3.0啥时候出", isatme: true}
	if _, ok := r.SearchQuery("<search 鸣潮 3.0 上线时间>"); ok {
		t.Fatal("search must be disabled unless enabled")
	}
	r.EnableSearch()
	if !strings.Contains(r.persona, "<search 搜索词>") {
		t.Fatal("search rules belong in the persona (stable per group, cache friendly)")
	}
	q, ok := r.SearchQuery("我查查 <Search 鸣潮 3.0 上线时间 >")
	if !ok || q != "鸣潮 3.0 上线时间" {
		t.Fatalf("got %q %v", q, ok)
	}
	if _, ok := r.SearchQuery("鸣潮3.0大概十月吧"); ok {
		t.Fatal("plain reply is not a search")
	}
	r.AddGroupRules(false)
	if !strings.Contains(r.finalUser(), freshHint) {
		t.Fatal("time-sensitive questions must get a search nudge")
	}
	if strings.Contains((&Request{text: "鸣潮3.0啥时候出"}).finalUser(), freshHint) {
		t.Fatal("no nudge when search is off")
	}
	if strings.Contains((&Request{text: "早上好", cansearch: true}).finalUser(), freshHint) {
		t.Fatal("no nudge for plain chat")
	}
	r.SetSearchResult(q, "1. 鸣潮3.0版本前瞻：10月16日上线")
	if strings.Contains(r.finalUser(), freshHint) {
		t.Fatal("no nudge after searching")
	}
	fu := r.finalUser()
	is, ic := strings.Index(fu, "【你刚在网上搜了「鸣潮 3.0 上线时间」】"), strings.Index(fu, "【当前消息】")
	if is < ic || !strings.Contains(fu, "10月16日上线") || !strings.Contains(fu, "不要再输出 <search>") || !strings.HasSuffix(fu, reminder) {
		t.Fatalf("search results go after the current message, before the reminder:\n%s", fu)
	}
	if _, ok := r.SearchQuery("<search 再搜一次>"); ok {
		t.Fatal("only one search per request")
	}
	r2 := &Request{}
	r2.SetSearchResult("x", "")
	if !strings.Contains(r2.searched, "没搜到") {
		t.Fatal("empty results must tell the model nothing was found")
	}
	if got := StripSearch("十月中旬吧<search 鸣潮>"); got != "十月中旬吧" {
		t.Fatalf("leftover tags must never be sent, got %q", got)
	}
}
