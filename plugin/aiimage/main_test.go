package aiimage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sql "github.com/FloatTech/sqlite"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"
)

func TestParseArgs(t *testing.T) {
	for in, want := range map[string][2]string{
		"一只猫":         {"", "一只猫"},
		"竖 一只猫":       {sizePortrait, "一只猫"},
		"竖图一只猫":       {sizePortrait, "一只猫"},
		"横，海边日落":      {sizeLandscape, "海边日落"},
		"横屏 赛博朋克街道":   {sizeLandscape, "赛博朋克街道"},
		"横店影视城的古装剧组":  {"", "横店影视城的古装剧组"},
		"竖琴演奏家":       {"", "竖琴演奏家"},
		"  方  水墨山水  ": {sizeSquare, "水墨山水"},
		"竖":           {sizePortrait, ""},
		"":            {"", ""},
	} {
		size, p := parseArgs(in)
		if size != want[0] || p != want[1] {
			t.Errorf("parseArgs(%q) = %q %q, want %q %q", in, size, p, want[0], want[1])
		}
	}
	if _, p := parseArgs(strings.Repeat("猫", 1500)); len([]rune(p)) != promptLen {
		t.Fatal("prompt must be capped")
	}
}

func TestCmdRuleAndInputs(t *testing.T) {
	img := message.Segment{Type: "image", Data: map[string]string{"url": "https://multimedia.nt.qq.com.cn/download?a=1&amp;b=2"}}
	ctx := &zero.Ctx{Event: &zero.Event{Message: message.Message{img, message.Text(" AI画图 竖 换成冬天")}}, State: zero.State{}}
	if !cmdRule(ctx) || ctx.State["args"] != "竖 换成冬天" {
		t.Fatalf("image before the command must still match: %v", ctx.State["args"])
	}
	if u := inputImages(ctx); len(u) != 1 || u[0] != "https://multimedia.nt.qq.com.cn/download?a=1&b=2" {
		t.Fatalf("input image url: %v", u)
	}
	ctx.Event.Message = message.Message{message.Text("帮我AI画图")}
	if cmdRule(ctx) {
		t.Fatal("command must be at the start of the text")
	}
	many := message.Message{message.Text("AI画图 拼起来")}
	for i := 0; i < 6; i++ {
		many = append(many, img)
	}
	ctx.Event.Message = many
	if len(inputImages(ctx)) != maxInputs {
		t.Fatal("inputs are capped")
	}
}

func TestBuildBody(t *testing.T) {
	var b map[string]any
	_ = json.Unmarshal(buildBody("https://z1rry.cyou/v1/images/generations", "gpt-image-2.5-不超分", "猫", ""), &b)
	if b["model"] != "gpt-image-2.5-不超分" || b["size"] != sizeSquare || b["n"] != 1.0 || len(b) != 4 {
		t.Fatalf("openai body must only carry standard params, square by default: %v", b)
	}
	b = nil
	_ = json.Unmarshal(buildBody("https://api.siliconflow.cn/v1/images/generations", "Kwai-Kolors/Kolors", "猫", sizePortrait), &b)
	if b["image_size"] != sizeSquare || b["batch_size"] != 1.0 || b["size"] != nil {
		t.Fatalf("siliconflow keeps its own params, one image: %v", b)
	}
}

func TestEditURL(t *testing.T) {
	if u, err := editURL("https://z1rry.cyou/v1/images/generations"); err != nil || u != "https://z1rry.cyou/v1/images/edits" {
		t.Fatalf("got %q %v", u, err)
	}
	if _, err := editURL("https://z1rry.cyou/v1"); !errors.Is(err, errNoEditURL) {
		t.Fatal("base url without endpoint cannot be mapped")
	}
}

func TestNormalizeInput(t *testing.T) {
	if im, err := normalizeInput([]byte("\x89PNG\r\n\x1a\n...")); err != nil || im.mime != "image/png" {
		t.Fatal("png")
	}
	if im, err := normalizeInput([]byte("\xff\xd8\xff\xe0...")); err != nil || im.ext != "jpg" {
		t.Fatal("jpeg")
	}
	if im, err := normalizeInput([]byte("RIFF\x00\x00\x00\x00WEBPVP8 ")); err != nil || im.mime != "image/webp" {
		t.Fatal("webp")
	}
	var g bytes.Buffer
	p := image.NewPaletted(image.Rect(0, 0, 4, 4), []color.Color{color.Black, color.White})
	if err := gif.EncodeAll(&g, &gif.GIF{Image: []*image.Paletted{p}, Delay: []int{0}}); err != nil {
		t.Fatal(err)
	}
	if im, err := normalizeInput(g.Bytes()); err != nil || im.mime != "image/png" || !bytes.HasPrefix(im.data, []byte("\x89PNG")) {
		t.Fatalf("gif must become png: %v", err)
	}
	if _, err := normalizeInput([]byte("<html>")); !errors.Is(err, errBadInput) {
		t.Fatal("unknown format")
	}
	pngOf := func(w, h int) inImage {
		var b bytes.Buffer
		_ = png.Encode(&b, image.NewGray(image.Rect(0, 0, w, h)))
		return inImage{b.Bytes(), "image/png", "png"}
	}
	for _, c := range []struct {
		w, h int
		want string
	}{{1024, 1536, sizePortrait}, {1600, 900, sizeLandscape}, {1000, 1100, sizeSquare}} {
		if got := orient(pngOf(c.w, c.h)); got != c.want {
			t.Errorf("orient %dx%d = %s, want %s", c.w, c.h, got, c.want)
		}
	}
	if orient(inImage{data: []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")}) != sizeAuto {
		t.Fatal("unknown dimensions fall back to auto")
	}
	if qqImageURL("http://127.0.0.1:3080/x.png") || !qqImageURL("https://gchat.qpic.cn/a") {
		t.Fatal("only QQ image hosts are fetched")
	}
}

func fakeAPI(t *testing.T, png []byte, got *map[string]any, files *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/img.png":
			_, _ = w.Write(png)
			return
		case "/v1/images/generations", "/v1/images/edits":
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid token","type":"new_api_error"}}`))
			return
		}
		m := map[string]any{}
		if strings.HasSuffix(r.URL.Path, "/edits") {
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := mr.NextPart()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(part)
				if part.FileName() != "" {
					*files = append(*files, part.FormName()+":"+part.FileName()+":"+part.Header.Get("Content-Type"))
					continue
				}
				m[part.FormName()] = string(b)
			}
		} else {
			_ = json.NewDecoder(r.Body).Decode(&m)
		}
		*got = m
		switch m["prompt"] {
		case "url":
			_, _ = w.Write([]byte(`{"data":[{"url":"` + "http://" + r.Host + `/img.png"}]}`))
		case "nsfw":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Your request was rejected as a result of our safety system."}}`))
		case "empty":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			_, _ = w.Write([]byte(`{"data":[{"b64_json":"` + base64.StdEncoding.EncodeToString(png) + `"}]}`))
		}
	}))
}

func TestGenerateAndEdit(t *testing.T) {
	png := []byte("\x89PNG fake")
	var got map[string]any
	var files []string
	srv := fakeAPI(t, png, &got, &files)
	defer srv.Close()
	cfg := imageConfig{APIKey: "sk-test", APIURL: srv.URL + "/v1/images/generations", ModelName: "gpt-image-2.5-不超分"}
	want := "base64://" + base64.StdEncoding.EncodeToString(png)

	img, err := generate(cfg, "一只猫", sizePortrait)
	if err != nil || img != want || got["size"] != sizePortrait {
		t.Fatalf("b64 response: %v %q %v", err, img, got)
	}
	if img, err = generate(cfg, "url", ""); err != nil || img != want {
		t.Fatalf("url response must be downloaded and sent as base64: %v %q", err, img)
	}
	if _, err = generate(cfg, "nsfw", ""); err == nil || !strings.Contains(errText(err), "没通过审核") {
		t.Fatalf("moderation: %v", err)
	}
	if _, err = generate(cfg, "empty", ""); !errors.Is(err, errNoImage) {
		t.Fatalf("empty: %v", err)
	}

	old := fetchInputs
	defer func() { fetchInputs = old }()
	fetchInputs = func(urls []string) ([]inImage, error) {
		ims := make([]inImage, 0, len(urls))
		for range urls {
			ims = append(ims, inImage{[]byte("\xff\xd8\xff fake"), "image/jpeg", "jpg"})
		}
		return ims, nil
	}
	img, err = edit(cfg, "换成冬天", "", []string{"u1"})
	if err != nil || img != want || got["model"] != "gpt-image-2.5-不超分" || got["size"] != sizeAuto || got["n"] != "1" {
		t.Fatalf("edit: %v %q %v", err, img, got)
	}
	if len(files) != 1 || files[0] != "image:input1.jpg:image/jpeg" {
		t.Fatalf("single input goes in field image: %v", files)
	}
	files = nil
	if _, err = edit(cfg, "拼在一起", sizeLandscape, []string{"u1", "u2"}); err != nil || got["size"] != sizeLandscape {
		t.Fatalf("multi edit: %v %v", err, got)
	}
	if len(files) != 2 || !strings.HasPrefix(files[1], "image[]:input2.jpg") {
		t.Fatalf("several inputs go in image[]: %v", files)
	}
	fetchInputs = func([]string) ([]inImage, error) { return nil, errBadInput }
	if _, err = edit(cfg, "x", "", []string{"u1"}); !strings.Contains(errText(err), "参考图") {
		t.Fatalf("bad input: %v", err)
	}

	cfg.APIKey = "bad"
	_, err = generate(cfg, "一只猫", "")
	if classify(err) != kindAuth || !strings.Contains(err.Error(), "new_api_error") {
		t.Fatalf("auth: %v %s", err, classify(err))
	}
}

type fakeTimeout struct{}

func (fakeTimeout) Error() string   { return "Client.Timeout exceeded" }
func (fakeTimeout) Timeout() bool   { return true }
func (fakeTimeout) Temporary() bool { return true }

func TestClassify(t *testing.T) {
	ae := func(status int, body string) error {
		_, _, e := parseResponse([]byte(body))
		if e == nil {
			e = &apiError{msg: body}
		}
		e.status = status
		return e
	}
	for _, c := range []struct {
		err  error
		want errKind
	}{
		// 实测: 中转站上不存在的模型
		{ae(503, `{"error":{"code":"model_not_found","message":"分组 【生图】1K 2K 4K 下模型 gpt-image-2.5-不超分-不存在 无可用渠道（distributor）","type":"new_api_error"}}`), kindModel},
		// new-api 余额不足是 403, 不能当成密钥问题
		{ae(403, `{"error":{"code":"insufficient_user_quota","message":"用户额度不足, 剩余额度: ＄0.0012","type":"new_api_error"}}`), kindQuota},
		{ae(403, `{"error":{"code":"pre_consume_token_quota_failed","message":"预扣费额度失败, 用户剩余额度: ＄0.01, 需要预扣费额度: ＄0.19","type":"new_api_error"}}`), kindQuota},
		{ae(401, `{"error":{"message":"该令牌额度已用尽 TokenStatusExhausted[sk-a***bc12]","type":"new_api_error"}}`), kindQuota},
		// OpenAI 额度用完是 429, 不能当成太忙
		{ae(429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","code":"insufficient_quota"}}`), kindQuota},
		{ae(401, `{"error":{"code":"","message":"Invalid token (request id: 2026…)","type":"new_api_error"}}`), kindAuth},
		{ae(401, `{"error":{"message":"该令牌已过期","type":"new_api_error"}}`), kindAuth},
		{ae(400, `{"error":{"message":"Your request was rejected by the safety system. If you believe this is an error, contact us.","type":"image_generation_user_error","code":"moderation_blocked"}}`), kindModeration},
		{ae(400, `{"error":{"message":"生成内容涉及版权角色，请修改提示词","type":"upstream_error"}}`), kindModeration},
		{ae(400, `{"error":{"message":"This request has been blocked by our content filters.","code":"content_policy_violation"}}`), kindModeration},
		{ae(503, `{"error":{"message":"当前分组上游负载已饱和，请稍后再试","type":"new_api_error"}}`), kindBusy},
		{ae(429, `{"error":{"message":"Rate limit reached for images per minute","code":"rate_limit_exceeded"}}`), kindBusy},
		{ae(502, `<html>502 Bad Gateway</html>`), kindUpstream},
		{ae(404, `404 page not found`), kindConfig},
		{ae(400, `{"error":{"message":"Invalid value: '123x456'. Supported values are: '1024x1024', '1024x1536'"}}`), kindOther},
		{&url.Error{Op: "Post", URL: "https://z1rry.cyou", Err: fakeTimeout{}}, kindTimeout},
		{&url.Error{Op: "Post", URL: "https://z1rry.cyou", Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}, kindNetwork},
		{errNoImage, kindNoImage},
		{errBadInput, kindBadInput},
		{errNoEditURL, kindConfig},
		{errors.New("x"), kindOther},
	} {
		if got := classify(c.err); got != c.want {
			t.Errorf("classify(%v) = %s, want %s", c.err, got, c.want)
		}
		if errText(c.err) == "" {
			t.Errorf("no text for %v", c.err)
		}
	}
	if s := errText(ae(400, `{"error":{"message":"Invalid value: '123x456'"}}`)); !strings.Contains(s, "123x456") {
		t.Fatalf("unknown errors show the relay's message: %q", s)
	}
	// 只有要主人处理的才私聊主人
	for k, want := range map[errKind]bool{kindQuota: true, kindAuth: true, kindModel: true, kindConfig: true,
		kindModeration: false, kindBusy: false, kindTimeout: false, kindBadInput: false} {
		if (ownerNote(k) != "") != want {
			t.Errorf("ownerNote(%s) notify=%v, want %v", k, !want, want)
		}
	}
	now := time.Now()
	if !shouldNote(kindQuota, now) || shouldNote(kindQuota, now.Add(10*time.Minute)) || !shouldNote(kindAuth, now) {
		t.Fatal("same kind is reported once per 30 min, kinds are independent")
	}
	if !shouldNote(kindQuota, now.Add(31*time.Minute)) {
		t.Fatal("reported again after 30 min")
	}
}

func TestReserveRefundAndConfig(t *testing.T) {
	sdb := &storage{db: sql.New(filepath.Join(t.TempDir(), "aiimage.db"))}
	if err := sdb.db.Open(time.Hour); err != nil {
		t.Fatal(err)
	}
	defer sdb.db.Close()
	if err := sdb.db.Create("config", &imageConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := sdb.db.Create("usage", &usage{}); err != nil {
		t.Fatal(err)
	}
	// 北京时间 10-30 23:59 与 10-31 00:01
	d1 := time.Date(2026, 10, 30, 15, 59, 0, 0, time.UTC)
	d2 := d1.Add(2 * time.Minute)
	var day int64
	for i := int64(1); i <= dailyLimit; i++ {
		var ok bool
		day, ok = sdb.reserve(7, d1)
		if !ok || sdb.used(7, d1) != i {
			t.Fatalf("reserve %d: ok=%v used=%d", i, ok, sdb.used(7, d1))
		}
	}
	if _, ok := sdb.reserve(7, d1); ok {
		t.Fatal("parallel requests must not exceed the daily limit")
	}
	sdb.refund(7, day)
	if sdb.used(7, d1) != dailyLimit-1 {
		t.Fatal("a failed image gives its slot back")
	}
	if _, ok := sdb.reserve(7, d1); !ok {
		t.Fatal("refunded slot can be used again")
	}
	if sdb.used(8, d1) != 0 || sdb.used(7, d2) != 0 {
		t.Fatal("usage is per user and resets at Beijing midnight")
	}
	if _, ok := sdb.reserve(7, d2); !ok || sdb.used(7, d2) != 1 {
		t.Fatal("new day starts from zero")
	}
	sdb.refund(7, day) // 昨天的单今天才失败: 不能扣今天的
	if sdb.used(7, d2) != 1 {
		t.Fatal("refund for a previous day must not touch today")
	}

	if err := sdb.setConfig("sk-abcdefghijklmnop1234", "https://z1rry.cyou/v1/images/generations", "gpt-image-2.5-不超分"); err != nil {
		t.Fatal(err)
	}
	s := sdb.PrintConfig()
	if strings.Contains(s, "abcdefghijklmnop") || !strings.Contains(s, "sk-a") || !strings.Contains(s, "1234") {
		t.Fatalf("key must be masked: %s", s)
	}
}
