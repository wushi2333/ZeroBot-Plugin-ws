// Package aiimage AI画图
package aiimage

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	_ "image/jpeg" // 让 image.DecodeConfig 认得 jpeg 参考图
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	fcext "github.com/FloatTech/floatbox/ctxext"
	sql "github.com/FloatTech/sqlite"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"

	ctrl "github.com/FloatTech/zbpctrl"
	"github.com/FloatTech/zbputils/control"
)

const (
	cmd         = "AI画图"
	dailyLimit  = 4 // 每人每天能画几张, 主人不限
	maxInflight = 8 // 全局同时最多画几张, 保护中转站和服务器内存
	maxInputs   = 4 // 图生图最多参考几张图
	promptLen   = 1000
	// genTimeout 生成一张图的时限. 在后台协程里画, 不受 ZeroBot 4 分钟处理时限的影响
	genTimeout = 5 * time.Minute
	dlTimeout  = 60 * time.Second
	inTimeout  = 30 * time.Second
	maxBytes   = 32 << 20
	maxInBytes = 20 << 20

	sizeSquare    = "1024x1024"
	sizePortrait  = "1024x1536"
	sizeLandscape = "1536x1024"
	sizeAuto      = "auto" // 图生图认不出原图比例时才用
)

var (
	genClient = &http.Client{Timeout: genTimeout}
	dlClient  = &http.Client{Timeout: dlTimeout}
	inClient  = &http.Client{
		Timeout: inTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !qqImageURL(req.URL.String()) {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	// slots 全局并发名额
	slots = make(chan struct{}, maxInflight)

	errNoImage    = errors.New("接口没有返回图片")
	errBadInput   = errors.New("参考图下载不了或格式不支持")
	errNoEditURL  = errors.New("接口地址要以 /images/generations 结尾才能图生图")
	errNoEditSupp = errors.New("当前画图接口不支持图生图")
)

// apiError 画图接口返回的错误
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string {
	return "HTTP " + strconv.Itoa(e.status) + ": " + e.msg
}

func init() {
	var sdb = &storage{}

	en := control.AutoRegister(&ctrl.Options[*zero.Ctx]{
		DisableOnDefault: false,
		Extra:            control.ExtraFromString("aiimage"),
		Brief:            "AI画图",
		Help: "- AI画图 [描述]\n" +
			"- AI画图 竖 [描述] / AI画图 横 [描述] (竖图 1024x1536, 横图 1536x1024, 默认方图)\n" +
			"- 图生图: 发图时带上 AI画图 [怎么改], 或者引用一条带图的消息再发 AI画图 [怎么改] (最多参考 " + strconv.Itoa(maxInputs) + " 张)\n" +
			"- 每人每天 " + strconv.Itoa(dailyLimit) + " 张 (画失败不算), 主人不限; 可以连着发, 画好一张发一张\n" +
			"- 设置AI画图密钥xxx\n" +
			"- 设置AI画图接口地址https://api.openai.com/v1/images/generations (也支持 SiliconFlow 文生图)\n" +
			"- 设置AI画图模型名gpt-image-1\n" +
			"- 查看AI画图配置",
		PrivateDataFolder: "aiimage",
	})

	getdb := fcext.DoOnceOnSuccess(func(ctx *zero.Ctx) bool {
		sdb.db = sql.New(en.DataFolder() + "aiimage.db")
		err := sdb.db.Open(time.Hour)
		if err == nil {
			// 创建配置表和用量表
			if err = sdb.db.Create("config", &imageConfig{}); err == nil {
				err = sdb.db.Create("usage", &usage{})
			}
			if err != nil {
				ctx.SendChain(message.Text("[ERROR]:", err))
				return false
			}
			return true
		}
		ctx.SendChain(message.Text("[ERROR]:", err))
		return false
	})

	en.OnPrefix("设置AI画图密钥", getdb, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			apiKey := strings.TrimSpace(ctx.State["args"].(string))
			cfg := sdb.getConfig()
			err := sdb.setConfig(apiKey, cfg.APIURL, cfg.ModelName)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: 设置API密钥失败: ", err))
				return
			}
			ctx.SendChain(message.Text("成功设置API密钥"))
		})

	en.OnPrefix("设置AI画图接口地址", getdb, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			apiURL := strings.TrimSpace(ctx.State["args"].(string))
			cfg := sdb.getConfig()
			err := sdb.setConfig(cfg.APIKey, apiURL, cfg.ModelName)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: 设置API地址失败: ", err))
				return
			}
			ctx.SendChain(message.Text("成功设置API地址"))
		})

	en.OnPrefix("设置AI画图模型名", getdb, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			modelName := strings.TrimSpace(ctx.State["args"].(string))
			cfg := sdb.getConfig()
			err := sdb.setConfig(cfg.APIKey, cfg.APIURL, modelName)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: 设置模型失败: ", err))
				return
			}
			ctx.SendChain(message.Text("成功设置模型: ", modelName))
		})

	en.OnFullMatch("查看AI画图配置", getdb, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			ctx.SendChain(message.Text(sdb.PrintConfig()))
		})

	// 不用 OnPrefix: 引用消息或带图发送时第一段不是文字, OnPrefix 匹配不到
	en.OnMessage(cmdRule, getdb).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			msgid := ctx.Event.MessageID
			reply := func(s string) { ctx.SendChain(message.Reply(msgid), message.Text(s)) }
			size, prompt := parseArgs(ctx.State["args"].(string))
			if prompt == "" {
				reply("说一下要画什么，比如：AI画图 竖 一只在窗边睡懒觉的橘猫\n想改图就带上图片或者引用一张图：AI画图 换成冬天下雪的样子")
				return
			}
			cfg := sdb.getConfig()
			if cfg.APIKey == "" || cfg.APIURL == "" || cfg.ModelName == "" {
				reply("AI画图还没配置好，需要主人先设置密钥、接口地址和模型名")
				return
			}
			inputs := inputImages(ctx)
			if len(inputs) > 0 && strings.Contains(cfg.APIURL, "siliconflow") {
				reply(errNoEditSupp.Error())
				return
			}
			uid, gid, owner := ctx.Event.UserID, ctx.Event.GroupID, zero.SuperUserPermission(ctx)
			var day, left int64
			if !owner {
				var ok bool
				if day, left, ok = sdb.reserve(uid, time.Now(), dailyLimit); !ok {
					reply("你今天的 " + strconv.Itoa(dailyLimit) + " 张已经画完啦，明天再来吧")
					return
				}
			}
			select {
			case slots <- struct{}{}:
			default:
				if !owner {
					sdb.refund(uid, day)
				}
				reply("现在一起画的人太多了，过一会儿再试试（这次不算次数）")
				return
			}
			ack := "收到，正在画"
			if len(inputs) > 0 {
				ack = "收到，正在照着这 " + strconv.Itoa(len(inputs)) + " 张图改"
			}
			ack += "，大概要一两分钟"
			if !owner {
				ack += "（今天还能再画 " + strconv.FormatInt(left, 10) + " 张）"
			}
			reply(ack)
			// 在后台画, 画好一张发一张; 处理器马上返回, 不占 ZeroBot 的处理时限
			go func() {
				defer func() { <-slots }()
				start := time.Now()
				var img string
				var err error
				if len(inputs) > 0 {
					img, err = edit(cfg, prompt, size, inputs)
				} else {
					img, err = generate(cfg, prompt, size)
				}
				if err != nil {
					if !owner {
						sdb.refund(uid, day)
					}
					logrus.Warnln("[aiimage] draw for", uid, "in", gid, "inputs", len(inputs), "failed after", time.Since(start).Round(time.Second), ":", err)
					reply(errText(err))
					return
				}
				ctx.SendChain(message.Reply(msgid), message.Image(img))
				logrus.Infoln("[aiimage] drew for", uid, "in", gid, "size", size, "inputs", len(inputs), "took", time.Since(start).Round(time.Second))
			}()
		})
}

// cmdRule 消息文字以 AI画图 开头 (允许前面有引用、@、图片), 其余文字放进 args
func cmdRule(ctx *zero.Ctx) bool {
	txt := strings.TrimSpace(ctx.ExtractPlainText())
	if !strings.HasPrefix(txt, cmd) {
		return false
	}
	ctx.State["args"] = strings.TrimSpace(txt[len(cmd):])
	return true
}

// parseArgs 取出开头的 横/竖/方 并返回尺寸和描述; 没指定尺寸时 size 为空.
// 单字前缀后面要有空格或标点, 免得把"横店""竖琴"吃掉
func parseArgs(s string) (size, prompt string) {
	s = strings.TrimSpace(s)
	for _, p := range []struct {
		word string
		size string
		sep  bool
	}{
		{"竖图", sizePortrait, false}, {"竖屏", sizePortrait, false}, {"横图", sizeLandscape, false},
		{"横屏", sizeLandscape, false}, {"方图", sizeSquare, false},
		{"竖", sizePortrait, true}, {"横", sizeLandscape, true}, {"方", sizeSquare, true},
	} {
		if !strings.HasPrefix(s, p.word) {
			continue
		}
		rest := s[len(p.word):]
		if p.sep {
			r, _ := utf8.DecodeRuneInString(rest)
			if rest != "" && !strings.ContainsRune(" \t\n　,，:：", r) {
				continue
			}
		}
		size, s = p.size, strings.TrimLeft(rest, " \t\n　,，:：")
		break
	}
	if utf8.RuneCountInString(s) > promptLen {
		s = string([]rune(s)[:promptLen])
	}
	return size, s
}

// ---------- 参考图 ----------

// inputImages 当前消息和被引用消息里的图片地址 (最多 maxInputs 张)
func inputImages(ctx *zero.Ctx) []string {
	out := make([]string, 0, maxInputs)
	add := func(m message.Message) {
		for _, seg := range m {
			if seg.Type == "image" && len(out) < maxInputs {
				if u := strings.ReplaceAll(seg.Data["url"], "&amp;", "&"); u != "" {
					out = append(out, u)
				}
			}
		}
	}
	add(ctx.Event.Message)
	for _, seg := range ctx.Event.Message {
		if seg.Type != "reply" {
			continue
		}
		if id, err := strconv.ParseInt(seg.Data["id"], 10, 64); err == nil {
			add(ctx.GetMessage(id, true).Elements)
		}
		break
	}
	return out
}

// qqImageURL 只下载 QQ 自己图床上的图
func qqImageURL(u string) bool {
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") {
		return false
	}
	h := "." + strings.ToLower(pu.Hostname())
	for _, s := range []string{".qq.com", ".qq.com.cn", ".qpic.cn", ".gtimg.cn"} {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

type inImage struct {
	data []byte
	mime string
	ext  string
}

// fetchInput 下载一张参考图, 只留接口支持的 png/jpeg/webp; gif 取第一帧转成 png
func fetchInput(u string, get func(string) ([]byte, error)) (inImage, error) {
	b, err := get(u)
	if err != nil {
		return inImage{}, err
	}
	return normalizeInput(b)
}

func normalizeInput(b []byte) (inImage, error) {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG")):
		return inImage{b, "image/png", "png"}, nil
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return inImage{b, "image/jpeg", "jpg"}, nil
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return inImage{b, "image/webp", "webp"}, nil
	case bytes.HasPrefix(b, []byte("GIF8")):
		g, err := gif.Decode(bytes.NewReader(b))
		if err != nil {
			return inImage{}, err
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, g); err != nil {
			return inImage{}, err
		}
		return inImage{buf.Bytes(), "image/png", "png"}, nil
	}
	return inImage{}, errBadInput
}

func getQQImage(u string) ([]byte, error) {
	if !qqImageURL(u) {
		return nil, errBadInput
	}
	resp, err := inClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxInBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxInBytes {
		return nil, errors.New("image too large")
	}
	return b, nil
}

// fetchInputs 用于测试替换
var fetchInputs = func(urls []string) ([]inImage, error) {
	ims := make([]inImage, 0, len(urls))
	for _, u := range urls {
		im, err := fetchInput(u, getQQImage)
		if err != nil {
			logrus.Infoln("[aiimage] skip input image:", err)
			continue
		}
		ims = append(ims, im)
	}
	if len(ims) == 0 {
		return nil, errBadInput
	}
	return ims, nil
}

// ---------- 请求 ----------

// buildBody 默认按 OpenAI 的格式; SiliconFlow 用它自己的参数名
func buildBody(api, model, prompt, size string) []byte {
	var body map[string]any
	if strings.Contains(api, "siliconflow") {
		body = map[string]any{
			"model":               model,
			"prompt":              prompt,
			"image_size":          sizeSquare,
			"batch_size":          1,
			"num_inference_steps": 20,
			"guidance_scale":      7.5,
		}
	} else {
		if size == "" {
			size = sizeSquare
		}
		body = map[string]any{"model": model, "prompt": prompt, "size": size, "n": 1}
	}
	b, _ := json.Marshal(body)
	return b
}

// editURL 由文生图地址得到图生图地址 (OpenAI: /v1/images/edits)
func editURL(api string) (string, error) {
	if !strings.HasSuffix(api, "/images/generations") {
		return "", errNoEditURL
	}
	return strings.TrimSuffix(api, "/generations") + "/edits", nil
}

// orient 按参考图的长宽比选输出尺寸. 实测中转站对 size=auto 会出正方形图, 竖图被裁成方的
func orient(im inImage) string {
	c, _, err := image.DecodeConfig(bytes.NewReader(im.data))
	if err != nil || c.Width <= 0 || c.Height <= 0 {
		return sizeAuto
	}
	switch r := float64(c.Height) / float64(c.Width); {
	case r >= 1.2:
		return sizePortrait
	case r <= 1/1.2:
		return sizeLandscape
	default:
		return sizeSquare
	}
}

// buildEdit 图生图的 multipart 请求体; 没指定横竖时跟着第一张参考图的比例
func buildEdit(model, prompt, size string, ims []inImage) (*bytes.Buffer, string, error) {
	if size == "" && len(ims) > 0 {
		size = orient(ims[0])
	}
	if size == "" {
		size = sizeAuto
	}
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	for k, v := range map[string]string{"model": model, "prompt": prompt, "size": size, "n": "1"} {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	field := "image"
	if len(ims) > 1 {
		field = "image[]"
	}
	for i, im := range ims {
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="input%d.%s"`, field, i+1, im.ext))
		h.Set("Content-Type", im.mime)
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(im.data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf, w.FormDataContentType(), nil
}

// parseResponse 取第一张图: OpenAI 的 data[].b64_json / data[].url, 或 SiliconFlow 的 images[].url
func parseResponse(data []byte) (b64, url, errmsg string) {
	j := gjson.ParseBytes(data)
	if e := j.Get("error"); e.Exists() {
		if m := e.Get("message").String(); m != "" {
			return "", "", m
		}
		return "", "", e.String()
	}
	for _, key := range []string{"data", "images"} {
		first := j.Get(key + ".0")
		if !first.Exists() {
			continue
		}
		b64 = first.Get("b64_json").String()
		if i := strings.Index(b64, "base64,"); strings.HasPrefix(b64, "data:") && i >= 0 {
			b64 = b64[i+len("base64,"):]
		}
		return b64, first.Get("url").String(), ""
	}
	return "", "", ""
}

// generate 文生图, 返回可直接发送的图片 (base64:// 或 url)
func generate(cfg imageConfig, prompt, size string) (string, error) {
	req, err := http.NewRequest(http.MethodPost, cfg.APIURL, bytes.NewReader(buildBody(cfg.APIURL, cfg.ModelName, prompt, size)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	return send(cfg, req)
}

// edit 图生图
func edit(cfg imageConfig, prompt, size string, urls []string) (string, error) {
	api, err := editURL(cfg.APIURL)
	if err != nil {
		return "", err
	}
	ims, err := fetchInputs(urls)
	if err != nil {
		return "", err
	}
	body, ctype, err := buildEdit(cfg.ModelName, prompt, size, ims)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, api, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", ctype)
	return send(cfg, req)
}

func send(cfg imageConfig, req *http.Request) (string, error) {
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	resp, err := genClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return "", err
	}
	b64, url, errmsg := parseResponse(data)
	if resp.StatusCode != http.StatusOK || errmsg != "" {
		if errmsg == "" {
			errmsg = strings.TrimSpace(string(data))
		}
		return "", &apiError{status: resp.StatusCode, msg: errmsg}
	}
	if b64 != "" {
		return "base64://" + b64, nil
	}
	if url == "" {
		return "", errNoImage
	}
	// 自己下载再发, 不依赖协议端能不能访问这个地址; 下载失败再退回直接发 url
	if b, err := download(url); err == nil {
		return "base64://" + base64.StdEncoding.EncodeToString(b), nil
	}
	return url, nil
}

func download(url string) ([]byte, error) {
	resp, err := dlClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBytes {
		return nil, errors.New("image too large")
	}
	return b, nil
}

// errText 给群友看的失败原因 (失败不计次数)
func errText(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		m := strings.ToLower(ae.msg)
		switch {
		case ae.status == http.StatusUnauthorized || (ae.status == http.StatusForbidden && !strings.Contains(m, "safety")):
			return "画图接口的密钥不对或没有权限，需要主人检查一下设置（这次不算次数）"
		case strings.Contains(m, "moderation") || strings.Contains(m, "safety") || strings.Contains(m, "content_policy") ||
			strings.Contains(m, "content policy") || strings.Contains(m, "违规") || strings.Contains(m, "敏感"):
			return "这个描述或图片没通过审核，换个试试（这次不算次数）"
		case ae.status == http.StatusTooManyRequests || ae.status == http.StatusServiceUnavailable:
			return "画图接口现在太忙了，过会儿再试试（这次不算次数）"
		case ae.status == http.StatusNotFound || ae.status == http.StatusMethodNotAllowed:
			return "画图接口不支持这个操作（这次不算次数）"
		}
		msg := ae.msg
		if utf8.RuneCountInString(msg) > 80 {
			msg = string([]rune(msg)[:80]) + "…"
		}
		return "画图失败了：" + msg + "（这次不算次数）"
	}
	switch {
	case errors.Is(err, errNoImage):
		return "接口没有返回图片，过会儿再试试（这次不算次数）"
	case errors.Is(err, errBadInput):
		return "参考图下载不了或者格式不支持，换张图试试（这次不算次数）"
	case errors.Is(err, errNoEditURL):
		return errNoEditURL.Error() + "（这次不算次数）"
	case strings.Contains(err.Error(), "Client.Timeout") || strings.Contains(err.Error(), "deadline exceeded"):
		return "画了 5 分钟还没画好，超时了，过会儿再试试（这次不算次数）"
	}
	return "画图失败了，过会儿再试试（这次不算次数）"
}
