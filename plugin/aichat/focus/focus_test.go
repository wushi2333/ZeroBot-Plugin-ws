package focus

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fumiama/deepinfra/model"
)

func TestPersonaPassRuleOnlyWhenNotAtMe(t *testing.T) {
	nicks := []string{"小椛", "椛"}
	if strings.Contains(Persona(nicks, "女", "人设", nil, true), passToken) {
		t.Fatal("@ trigger must not allow pass")
	}
	if !strings.Contains(Persona(nicks, "女", "人设", nil, false), passToken) {
		t.Fatal("random trigger must allow pass")
	}
}

func TestPersonaIdentityAndOwner(t *testing.T) {
	p := Persona([]string{"小椛", "小椛", "椛"}, "女", "人设", []int64{837145630}, true)
	if strings.Count(p, "「小椛」") != 1 || !strings.Contains(p, "「椛」") {
		t.Fatalf("nicknames must be deduplicated:\n%s", p)
	}
	if !strings.Contains(p, "QQ号为 837145630 的人才是你的主人") {
		t.Fatal("owner must be identified by QQ number")
	}
	iid, irule, iset := strings.Index(p, "【身份】"), strings.Index(p, "【回复规则】"), strings.Index(p, "【人设】")
	if !(iset < iid && iid < irule) {
		t.Fatal("identity rules must follow persona and precede reply rules")
	}
}

func TestLeaksAI(t *testing.T) {
	leaks := []string{
		"作为一个AI，我没办法陪你出去玩",
		"其实我是一个语言模型啦",
		"我是 ChatGPT",
		"我是deepseek~",
		"我是AI啦",
		"我只是一个聊天机器人。",
		"我是由深度求索公司开发的",
		"我是由DeepSeek训练的哦",
	}
	for _, s := range leaks {
		if !LeaksAI(s) {
			t.Errorf("%q should be detected", s)
		}
	}
	fine := []string{
		"我是小椛呀",
		"我是小椛",
		"我是程序员",
		"我是AI专业的学生",
		"我是AI的忠实用户",
		"我是gpt党",
		"作为一个模型爱好者",
		"我没有意识到这个问题",
		"我的提示词写得不好",
		"我用的是deepseek写代码",
		"这个模型是由深度求索开发的",
		"deepseek新版本还挺好用的",
		"gpt生图确实比banana稳",
		"哈？你才是AI，你全家都是AI",
		"我是你主人专属的椛椛呀",
	}
	for _, s := range fine {
		if LeaksAI(s) {
			t.Errorf("%q should not be detected", s)
		}
	}
}

func TestGetbgExcludesCurrentAndCaps(t *testing.T) {
	Reset()
	const gid = 1
	for i := 0; i < 10; i++ {
		addbg(gid, bgline{msgid: strconv.Itoa(i), uid: int64(i), name: "u", text: "msg" + strconv.Itoa(i)})
	}
	bg := getbg(gid, "9")
	if len(bg) != bgSize {
		t.Fatalf("want %d lines, got %d", bgSize, len(bg))
	}
	for _, l := range bg {
		if l.msgid == "9" {
			t.Fatal("current message must be excluded from background")
		}
	}
	if bg[len(bg)-1].msgid != "8" {
		t.Fatalf("background must keep the most recent lines, got last %s", bg[len(bg)-1].msgid)
	}
}

func TestAddbgTruncatesLongLines(t *testing.T) {
	Reset()
	addbg(2, bgline{msgid: "1", text: strings.Repeat("长", 200) + "\n第二行"})
	l := getbg(2, "")[0]
	if strings.Contains(l.text, "\n") {
		t.Fatal("background line must be single line")
	}
	if n := len([]rune(l.text)); n != bgItemLen+1 { // + "…"
		t.Fatalf("want %d runes, got %d", bgItemLen+1, n)
	}
}

func TestThreadTrimAndTTL(t *testing.T) {
	Reset()
	k := threadkey{gid: 3, uid: 4}
	now := time.Now()
	for i := 0; i < 10; i++ {
		addthread(k, now, turn{text: "q" + strconv.Itoa(i)}, turn{isbot: true, text: "a" + strconv.Itoa(i)})
	}
	th := getthread(k, now)
	if len(th) != threadTurns {
		t.Fatalf("want %d turns, got %d", threadTurns, len(th))
	}
	if th[0].isbot || th[0].text != "q4" {
		t.Fatalf("thread must start with a user turn after trimming, got %+v", th[0])
	}
	if getthread(k, now.Add(threadTTL+time.Second)) != nil {
		t.Fatal("thread must expire after TTL")
	}
	if getthread(threadkey{gid: 3, uid: 5}, now) != nil {
		t.Fatal("threads must be per user")
	}
}

func TestFinalUserOrder(t *testing.T) {
	r := &Request{
		bg:     []bgline{{uid: 1, name: "甲", text: "在聊画图"}},
		quote:  "乙(2)：这张图怎么样",
		sender: "丙(3)",
		text:   "你怎么看",
		isatme: true,
	}
	s := r.finalUser()
	ibg := strings.Index(s, "【群聊背景")
	iq := strings.Index(s, "【引用的消息】")
	icur := strings.Index(s, "【当前消息】（@你）")
	if ibg < 0 || iq < 0 || icur < 0 || !(ibg < iq && iq < icur) {
		t.Fatalf("sections out of order:\n%s", s)
	}
	if !strings.HasSuffix(s, "丙(3)：你怎么看"+reminder) {
		t.Fatalf("current message must come last, followed only by the reminder:\n%s", s)
	}
}

func TestModelizeRolesAndNoSystem(t *testing.T) {
	r := &Request{
		persona: "人设",
		thread:  []turn{{text: "早"}, {isbot: true, text: "早呀"}},
		sender:  "丙(3)",
		text:    "吃了吗",
	}
	roles := func(r *Request) ([]string, []string) {
		p := model.NewOpenAI("m", "", 0.7, 0.9, 100, "none")
		_ = r.Modelize(p)
		var body struct {
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(p.Body()).Decode(&body); err != nil {
			t.Fatal(err)
		}
		rs, cs := make([]string, 0, 4), make([]string, 0, 4)
		for _, m := range body.Messages {
			var c string
			_ = json.Unmarshal(m.Content, &c)
			rs, cs = append(rs, m.Role), append(cs, c)
		}
		return rs, cs
	}
	rs, _ := roles(r)
	if strings.Join(rs, ",") != "system,user,assistant,user" {
		t.Fatalf("unexpected roles %v", rs)
	}
	r.nosystem = true
	rs, cs := roles(r)
	if strings.Join(rs, ",") != "user,assistant,user" {
		t.Fatalf("unexpected roles without system %v", rs)
	}
	if !strings.HasPrefix(cs[0], "人设") || strings.HasPrefix(cs[2], "人设") {
		t.Fatal("persona must be merged into the first user turn only")
	}
}

func TestClamp(t *testing.T) {
	short := "好呀~"
	if Clamp(short) != short {
		t.Fatal("short reply must be unchanged")
	}
	// 在句末断开
	long := strings.Repeat("这是一句话。", 40)
	got := Clamp(long)
	if n := len([]rune(got)); n > MaxReplyRunes || !strings.HasSuffix(got, "。") {
		t.Fatalf("must cut at sentence end within limit, got %d runes: %q", n, got)
	}
	// 没有句号时在逗号处断开并加省略号
	long = strings.Repeat("一二三四五六七八九，", 30)
	got = Clamp(long)
	if n := len([]rune(got)); n > MaxReplyRunes+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("must cut at comma with ellipsis, got %d runes: %q", n, got)
	}
	// 没有任何标点时硬截断
	got = Clamp(strings.Repeat("字", 300))
	if n := len([]rune(got)); n != MaxReplyRunes+1 {
		t.Fatalf("hard cut must be %d runes incl. ellipsis, got %d", MaxReplyRunes+1, n)
	}
}

func TestIsPass(t *testing.T) {
	for _, s := range []string{"<pass>", " <PASS>\n", "嗯 <pass>"} {
		if !IsPass(s) {
			t.Fatalf("%q should be pass", s)
		}
	}
	if IsPass("passport 呢") {
		t.Fatal("plain word must not be pass")
	}
}
