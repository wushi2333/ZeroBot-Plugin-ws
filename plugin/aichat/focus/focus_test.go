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
	if strings.Contains(Persona("小椛", "女", "人设", true), passToken) {
		t.Fatal("@ trigger must not allow pass")
	}
	if !strings.Contains(Persona("小椛", "女", "人设", false), passToken) {
		t.Fatal("random trigger must allow pass")
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
	if !strings.HasSuffix(s, "丙(3)：你怎么看") {
		t.Fatalf("current message must come last:\n%s", s)
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
