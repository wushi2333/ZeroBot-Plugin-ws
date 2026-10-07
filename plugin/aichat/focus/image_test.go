package focus

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fumiama/deepinfra/model"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"

	"github.com/FloatTech/zbputils/ctxext"
)

const qqimg = "https://multimedia.nt.qq.com.cn/download?appid=1407&amp;fileid=abc"

func imgseg(u, sub string) message.Segment {
	return message.Segment{Type: "image", Data: map[string]string{"url": u, "subType": sub}}
}

func TestAllowedImageURL(t *testing.T) {
	for u, want := range map[string]bool{
		"https://multimedia.nt.qq.com.cn/download?x=1":  true,
		"https://gchat.qpic.cn/gchatpic_new/1/2-3/0":    true,
		"https://gxh.vip.qq.com/club/item/parcel/a.png": true,
		"http://127.0.0.1:3080/x.png":                   false,
		"https://evilqq.com/a.png":                      false,
		"https://qq.com.evil.cn/a.png":                  false,
		"file:///etc/passwd":                            false,
		"":                                              false,
	} {
		if got := allowedImageURL(u); got != want {
			t.Errorf("allowedImageURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestRenderTextImages(t *testing.T) {
	ctx := &zero.Ctx{Event: &zero.Event{SelfID: 1, Message: message.Message{
		message.At(1),
		message.Text("这是啥"),
		imgseg(qqimg, "0"),
		imgseg(qqimg, "1"),
		{Type: "mface", Data: map[string]string{"url": "https://gxh.vip.qq.com/a.png", "summary": "[开心]"}},
		imgseg(qqimg, "0"),                     // 超出 3 张
		imgseg("http://127.0.0.1:3080/x", "0"), // 不是 QQ 图床
	}}, State: zero.State{}}
	set := &imgset{}
	txt, _, _ := renderText(ctx, set)
	if txt != "这是啥[图片1][表情2][表情3][图片][图片]" {
		t.Fatalf("unexpected placeholders %q", txt)
	}
	if len(set.refs) != maxImages || !strings.Contains(set.refs[0].url, "&fileid=") {
		t.Fatalf("refs must be capped and &amp; unescaped: %+v", set.refs)
	}
	if !HasImage(ctx) {
		t.Fatal("message with image must be detected")
	}
	ctx.Event.Message = message.Message{message.Text("hi"), {Type: "mface", Data: map[string]string{"summary": "[哭]"}}}
	if HasImage(ctx) {
		t.Fatal("mface without url is not an image")
	}
}

func TestOverLimitMfaceKeepsSummary(t *testing.T) {
	set := &imgset{refs: make([]imgref, maxImages)}
	if p := set.add(message.Segment{Type: "mface", Data: map[string]string{"url": "https://gxh.vip.qq.com/a.png", "summary": "[开心]"}}); p != "[表情：开心]" {
		t.Fatalf("got %q", p)
	}
}

func TestLoadImagesAndFinalContents(t *testing.T) {
	old := fetch
	defer func() { fetch = old }()
	fetch = func(u string) (string, error) {
		if strings.Contains(u, "bad") {
			return "", errors.New("404")
		}
		return "data:image/png;base64,AAAA", nil
	}
	r := &Request{
		persona: "人设", sender: "丙(3)", text: "看看[图片1][图片2]", isatme: true,
		images: []imgref{{label: "图片1", url: "https://gchat.qpic.cn/ok"}, {label: "图片2", url: "https://gchat.qpic.cn/bad"}},
	}
	r.AddGroupRules(false)
	r.LoadImages()
	if !r.HasImages() || !strings.Contains(r.finalBody(), "[图片2]没加载出来") {
		t.Fatalf("failed image must be noted, body:\n%s", r.finalBody())
	}
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
	var parts []model.Content
	if err := json.Unmarshal(body.Messages[len(body.Messages)-1].Content, &parts); err != nil {
		t.Fatal("final message with images must be content parts:", err)
	}
	// 正文 -> [图片1] -> 图 -> 结尾说明
	if len(parts) != 4 || parts[1].Text != "[图片1]" || parts[2].Type != model.ContentTypeImageURL ||
		!strings.HasSuffix(parts[0].Text, "看看[图片1][图片2]\n（[图片2]没加载出来，你看不到内容）") ||
		!strings.HasSuffix(parts[3].Text, reminder) || !strings.Contains(parts[3].Text, "不要假装已经禁言") {
		t.Fatalf("unexpected parts %+v", parts)
	}

	r.DropImages()
	if r.HasImages() || len(r.finalContents("")) != 1 || !strings.Contains(r.finalUser(), "图片没加载出来") {
		t.Fatal("dropping images must fall back to a single text part with a note")
	}
}

func TestTakeRecent(t *testing.T) {
	now := time.Now()
	k := threadkey{gid: 100, uid: 7}
	segs := []message.Segment{imgseg(qqimg, "0")}
	addrecent(k, "m1", segs, now)
	if got := takerecent(k, "m1", now); got != nil {
		t.Fatal("the current message itself is not a recent image")
	}
	if takerecent(k, "m2", now) != nil {
		t.Fatal("taking must consume the entry")
	}
	addrecent(k, "m1", segs, now)
	if takerecent(threadkey{gid: 100, uid: 8}, "m2", now) != nil {
		t.Fatal("recent images are per user")
	}
	if got := takerecent(k, "m2", now.Add(time.Minute)); len(got) != 1 {
		t.Fatal("same user's fresh image must be returned")
	}
	addrecent(k, "m1", segs, now)
	if takerecent(k, "m2", now.Add(recentImageTTL+time.Second)) != nil {
		t.Fatal("stale image must be ignored")
	}
}

func TestRecentSection(t *testing.T) {
	r := &Request{sender: "丙(3)", text: "这是啥", isatme: true, recent: "[图片1]"}
	b := r.finalBody()
	if strings.Index(b, "【TA刚才发的图") > strings.Index(b, "【当前消息】") {
		t.Fatal("recent image section must come before the current message")
	}
}

func TestEffort(t *testing.T) {
	var s ctxext.Storage
	if Effort(s) != "none" || MaxTokensFor(Effort(s)) != MaxTokens {
		t.Fatal("default must be no thinking with the short reply budget")
	}
	s = s.Set(1, BitmapEffort)
	if Effort(s) != "low" || !(MaxTokens < MaxTokensFor("low") && MaxTokensFor("low") < MaxTokensFor("high")) ||
		MaxTokensFor("high") > 4096 {
		t.Fatal("each level needs its own capped budget: none < low < high <= 4096")
	}
	s = s.Set(2, BitmapEffort)
	if Effort(s) != "high" {
		t.Fatal("2 is high")
	}
	s = s.Set(3, BitmapEffort)
	if Effort(s) != "none" {
		t.Fatal("unknown value falls back to none")
	}
	// 不能和其他位重叠
	for _, b := range []int64{0xff, 0xff00, 0x010000, 0x020000, 0x040000, BitmapNfcs, 0x100000, 0x200000} {
		if b&BitmapEffort != 0 {
			t.Fatalf("effort bits overlap %#x", b)
		}
	}
}
