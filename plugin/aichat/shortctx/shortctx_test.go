package shortctx

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
	"github.com/wdvxdr1123/ZeroBot/message"

	"github.com/FloatTech/zbputils/ctxext"
)

// usetempdb 让摘要库落在临时目录, 结束时关闭
func usetempdb(t *testing.T) {
	t.Helper()
	dbmu.Lock()
	dbpath = filepath.Join(t.TempDir(), "shortctx.db")
	dbinit = false
	dbmu.Unlock()
	t.Cleanup(func() {
		dbmu.Lock()
		defer dbmu.Unlock()
		if dbinit {
			_ = db.Close()
		}
		dbinit = false
	})
}

func resetwindows() {
	wmu.Lock()
	windows = map[int64]*window{}
	wmu.Unlock()
}

func TestEnabledDefaultOff(t *testing.T) {
	var s ctxext.Storage
	if Enabled(s) {
		t.Fatal("group context must be off by default")
	}
	if !Enabled(s.Set(1, BitmapGctx)) {
		t.Fatal("bit set means on")
	}
}

func TestRender(t *testing.T) {
	got := render(message.Message{
		message.At(42),
		message.Text("看这个 "),
		{Type: "at", Data: map[string]string{"qq": "1001", "name": "@RinDB"}},
		{Type: "image", Data: map[string]string{"url": "x"}},
		{Type: "mface", Data: map[string]string{"summary": "[开心]"}},
		{Type: "record"}, {Type: "video"}, {Type: "json"}, {Type: "reply", Data: map[string]string{"id": "1"}},
	}, 42)
	if got != "看这个 @RinDB [图片][表情][语音][视频][卡片]" {
		t.Fatalf("unexpected render %q", got)
	}
	long := render(message.Message{message.Text(strings.Repeat("字", 300))}, 0)
	if n := len([]rune(long)); n != lineLen+1 {
		t.Fatalf("line must be cut to %d runes + ellipsis, got %d", lineLen, n)
	}
}

func TestAddEvictsOnlyForEnabledGroups(t *testing.T) {
	resetwindows()
	oldEn := enabledGroup
	defer func() { enabledGroup = oldEn }()
	queued := map[int64]bool{}
	enabledGroup = func(gid int64) bool { queued[gid] = true; return false } // 记录调用, 但不真正入队
	for i := 0; i < windowMax+1; i++ {
		add(1, line{ts: int64(i), uid: 7, name: "甲", text: "第" + strconv.Itoa(i) + "条", msgid: strconv.Itoa(i)})
	}
	wmu.Lock()
	n, first := len(windows[1].lines), windows[1].lines[0].msgid
	wmu.Unlock()
	if n != windowMax+1-evictChunk || first != strconv.Itoa(evictChunk) {
		t.Fatalf("must evict the oldest %d lines, have %d first %s", evictChunk, n, first)
	}
	if !queued[1] {
		t.Fatal("eviction must check whether the group is enabled")
	}
}

func TestBuildExcludesCurrentAndKeepsPrefix(t *testing.T) {
	usetempdb(t)
	resetwindows()
	now := time.Now()
	ts := now.Unix()
	add(2, line{ts: ts - 60, uid: 7, name: "甲", text: "今晚打鸣潮", msgid: "a"})
	add(2, line{ts: ts - 30, uid: 3482259278, text: "我也来", self: true, msgid: "b"})
	add(2, line{ts: ts, uid: 8, name: "乙", text: "@椛椛 他刚才说啥", msgid: "cur"})
	s := build(2, "cur", now)
	if strings.Contains(s, "他刚才说啥") {
		t.Fatal("current message must be excluded")
	}
	if !strings.Contains(s, "甲(7)：今晚打鸣潮") || !strings.Contains(s, " 我：我也来") {
		t.Fatalf("lines must name speakers with QQ and self as 我:\n%s", s)
	}
	// 新消息只追加在末尾: 旧内容是新内容的前缀 (前缀缓存)
	add(2, line{ts: ts + 10, uid: 9, name: "丙", text: "带我一个", msgid: "d"})
	s2 := build(2, "", now)
	if !strings.HasPrefix(s2, s) {
		t.Fatalf("context must only grow at the end:\n%s\n---\n%s", s, s2)
	}
	if build(3, "", now) != "" {
		t.Fatal("empty group must produce no context")
	}
}

func TestParseHistory(t *testing.T) {
	js := `{"messages":[
		{"time":200,"message_id":2,"sender":{"user_id":8,"nickname":"乙nick","card":""},"message":[{"type":"text","data":{"text":"后说的"}}]},
		{"time":100,"message_id":1,"sender":{"user_id":7,"nickname":"甲nick","card":"甲"},"message":[{"type":"text","data":{"text":"先说的"}},{"type":"image","data":{"url":"x"}}]},
		{"time":150,"message_id":3,"sender":{"user_id":42,"nickname":"bot"},"message":[{"type":"text","data":{"text":"我说的"}}]},
		{"time":160,"message_id":4,"sender":{"user_id":9,"nickname":"丙"},"message":[{"type":"reply","data":{"id":"1"}}]}
	]}`
	ls := parseHistory(gjson.Parse(js), 42)
	if len(ls) != 3 {
		t.Fatalf("empty messages must be skipped, got %d", len(ls))
	}
	if ls[0].text != "先说的[图片]" || ls[0].name != "甲" || ls[0].msgid != "1" {
		t.Fatalf("must sort by time and prefer card: %+v", ls[0])
	}
	if !ls[1].self || ls[2].name != "乙nick" {
		t.Fatalf("self/nickname fallback wrong: %+v", ls)
	}
}

func TestSummarizeAndCompact(t *testing.T) {
	usetempdb(t)
	oldllm := llm
	defer func() { llm = oldllm }()
	var prompts []string
	llm = func(p string) (string, error) {
		prompts = append(prompts, p)
		return "大家约了今晚打鸣潮" + strconv.Itoa(len(prompts)), nil
	}
	now := time.Now()
	base := now.Add(-time.Hour).Unix()
	// 过期的摘要会被删掉
	dbmu.Lock()
	if err := opendb(); err != nil {
		t.Fatal(err)
	}
	_ = db.Insert(tableSummary, &Summary{ID: newid(), GID: 5, Start: now.Add(-14 * time.Hour).Unix(), End: now.Add(-13 * time.Hour).Unix(), Text: "很久以前"})
	dbmu.Unlock()

	for i := 0; i < summaryMax+1; i++ {
		lines := []line{
			{ts: base + int64(i*60), uid: 7, name: "甲", text: "第" + strconv.Itoa(i) + "段"},
			{ts: base + int64(i*60+30), uid: 42, text: "嗯", self: true},
		}
		if err := summarize(5, lines, now); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(prompts[0], "甲(7)：第0段") || !strings.Contains(prompts[0], " 我：嗯") {
		t.Fatalf("summary prompt must contain the lines:\n%s", prompts[0])
	}
	ss := summaries(5, now)
	if len(ss) > summaryMax {
		t.Fatalf("must merge down to at most %d summaries, have %d", summaryMax, len(ss))
	}
	last := prompts[len(prompts)-1]
	if !strings.Contains(last, "合并成一段") {
		t.Fatal("over the cap the oldest summaries must be merged by the llm")
	}
	for _, s := range ss {
		if s.Text == "很久以前" {
			t.Fatal("summaries older than 12h must be deleted")
		}
	}
	for i := 1; i < len(ss); i++ {
		if ss[i].Start < ss[i-1].Start {
			t.Fatal("summaries must be in time order")
		}
	}
	if !strings.HasPrefix(build(5, "", now), "【今天早些时候群里聊过】") {
		t.Fatal("summaries go first in the context")
	}
}
