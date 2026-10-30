// Package aiimage AI画图
package aiimage

import (
	"bytes"
	"context"
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
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// cmds 触发画图的指令; 群里有人习惯说"AI生图"
var cmds = []string{"AI画图", "AI生图"}

const (
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

// apiError 画图接口返回的错误 (new-api / OpenAI: {"error":{"message","type","code"}})
type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string {
	s := "HTTP " + strconv.Itoa(e.status)
	if e.code != "" {
		s += " [" + e.code + "]"
	}
	return s + ": " + e.msg
}

func init() {
	var sdb = &storage{}

	en := control.AutoRegister(&ctrl.Options[*zero.Ctx]{
		DisableOnDefault: false,
		Extra:            control.ExtraFromString("aiimage"),
		Brief:            "AI画图",
		Help: "- AI画图 [描述] (也可以说 AI生图)\n" +
			"- AI画图 竖 [描述] / AI画图 横 [描述] (竖图 1024x1536, 横图 1536x1024, 默认方图)\n" +
			"- 图生图: 发图时带上 AI画图 [怎么改], 或者引用一条带图的消息再发 AI画图 [怎么改] (最多参考 " + strconv.Itoa(maxInputs) + " 张)\n" +
			"- 多张参考图按顺序编号 (引用的图在前, 再按这条消息里的顺序), 例如一条消息发两张图: AI画图 用第一张图的姿势, 画第二张图里的角色\n" +
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
			var day int64
			if !owner {
				var ok bool
				if day, ok = sdb.reserve(uid, time.Now()); !ok {
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
			reply(ack + "，大概要一两分钟，画好了会@你")
			// 画完 (或失败) 时引用原消息并 @ 他; 剩几张按那时算, 正在画的也算用掉
			done := func(chain ...message.Segment) {
				head := message.Message{message.Reply(msgid)}
				if gid != 0 {
					head = append(head, message.At(uid), message.Text(" "))
				}
				ctx.SendChain(append(head, chain...)...)
			}
			leftText := func() string {
				if owner {
					return ""
				}
				if n := dailyLimit - sdb.used(uid, time.Now()); n > 0 {
					return "今天还能再画 " + strconv.FormatInt(n, 10) + " 张"
				}
				return "今天的 " + strconv.Itoa(dailyLimit) + " 张已经用完啦"
			}
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
					kind := classify(err)
					logrus.Warnln("[aiimage] draw for", uid, "in", gid, "inputs", len(inputs), "failed after", time.Since(start).Round(time.Second), "kind", kind, ":", err)
					text := errText(err) + "（这次不算次数"
					if l := leftText(); l != "" {
						text += "，" + l
					}
					text += "）"
					if owner { // 主人看得到原始报错, 方便排查
						text += "\n原始错误：" + cutRunes(err.Error(), 200)
					} else {
						notifyOwners(ctx, kind, err, gid, uid)
					}
					done(message.Text(text))
					return
				}
				chain := []message.Segment{message.Image(img)}
				if l := leftText(); l != "" {
					chain = append(chain, message.Text("画好啦，"+l))
				}
				done(chain...)
				logrus.Infoln("[aiimage] drew for", uid, "in", gid, "size", size, "inputs", len(inputs), "took", time.Since(start).Round(time.Second))
			}()
		})
}

// cmdRule 消息文字以 AI画图/AI生图 开头 (允许前面有引用、@、图片), 其余文字放进 args
func cmdRule(ctx *zero.Ctx) bool {
	txt := strings.TrimSpace(ctx.ExtractPlainText())
	for _, c := range cmds {
		if strings.HasPrefix(txt, c) {
			ctx.State["args"] = strings.TrimSpace(txt[len(c):])
			return true
		}
	}
	return false
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

// inputImages 参考图地址 (最多 maxInputs 张), 按时间先后排: 被引用消息里的图在前, 再是这条消息里的图.
// 描述里说的"第一张""第二张"就是这个顺序
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
	for _, seg := range ctx.Event.Message {
		if seg.Type != "reply" {
			continue
		}
		if id, err := strconv.ParseInt(seg.Data["id"], 10, 64); err == nil {
			add(ctx.GetMessage(id, true).Elements)
		}
		break
	}
	add(ctx.Event.Message)
	return out
}

// orderHint 多张参考图时告诉模型图的顺序, 让"第一张的姿势、第二张的角色"对得上
func orderHint(prompt string, n int) string {
	if n < 2 {
		return prompt
	}
	return prompt + "\n\n（参考图共 " + strconv.Itoa(n) + " 张，按上传顺序依次是第1张到第" + strconv.Itoa(n) +
		"张；上面说的“第一张”“第二张”“图1”“图2”就是指这个顺序。Reference images are given in order: image 1 to image " +
		strconv.Itoa(n) + ".）"
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

// normalizeInput 只留接口支持的 png/jpeg/webp; gif 取第一帧转成 png
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

// fetchInputs 下载全部参考图; 任何一张不行就整次失败, 不能跳过, 否则"第一张""第二张"会错位. 测试时可替换
var fetchInputs = func(urls []string) ([]inImage, error) {
	ims := make([]inImage, 0, len(urls))
	for i, u := range urls {
		b, err := getQQImage(u)
		if err == nil {
			var im inImage
			if im, err = normalizeInput(b); err == nil {
				ims = append(ims, im)
				continue
			}
		}
		logrus.Infoln("[aiimage] input image", i+1, "of", len(urls), "unusable:", err)
		return nil, errBadInput
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

// parseResponse 取第一张图: OpenAI 的 data[].b64_json / data[].url, 或 SiliconFlow 的 images[].url.
// 有 error 字段时返回其中的 code 和 message (status 由调用方填)
func parseResponse(data []byte) (b64, url string, ae *apiError) {
	j := gjson.ParseBytes(data)
	if e := j.Get("error"); e.Exists() {
		ae = &apiError{code: e.Get("code").String(), msg: e.Get("message").String()}
		if ae.code == "" {
			ae.code = e.Get("type").String()
		}
		if ae.msg == "" {
			ae.msg = e.String()
		}
		return "", "", ae
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
		return b64, first.Get("url").String(), nil
	}
	return "", "", nil
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
	body, ctype, err := buildEdit(cfg.ModelName, orderHint(prompt, len(ims)), size, ims)
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
	b64, url, ae := parseResponse(data)
	if resp.StatusCode != http.StatusOK || ae != nil {
		if ae == nil {
			ae = &apiError{msg: strings.TrimSpace(string(data))}
		}
		ae.status = resp.StatusCode
		return "", ae
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

// ---------- 失败处理 ----------

// errKind 失败的种类: 决定给群友看什么, 要不要私聊告诉主人
type errKind string

const (
	kindQuota      errKind = "quota"      // 中转站余额/额度不足
	kindAuth       errKind = "auth"       // 密钥无效/过期/被禁用
	kindModel      errKind = "model"      // 模型不可用 (无可用渠道)
	kindModeration errKind = "moderation" // 审核不通过 (敏感内容/版权角色)
	kindBusy       errKind = "busy"       // 限流/上游负载满
	kindUpstream   errKind = "upstream"   // 上游 5xx
	kindTimeout    errKind = "timeout"    // 5 分钟没画完
	kindNetwork    errKind = "network"    // 连不上接口
	kindNoImage    errKind = "noimage"    // 200 但没有图
	kindBadInput   errKind = "badinput"   // 参考图有问题
	kindConfig     errKind = "config"     // 配置问题 (接口地址)
	kindOther      errKind = "other"
)

func hasAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// classify 先看错误码, 再看报错文字, 最后看 HTTP 状态码. 顺序有讲究:
// new-api 余额不足是 403, OpenAI 额度用完是 429, 无可用渠道是 503, 都不能只看状态码
func classify(err error) errKind {
	var ae *apiError
	if errors.As(err, &ae) {
		m := strings.ToLower(ae.code + " " + ae.msg)
		switch {
		case hasAny(m, "insufficient_user_quota", "insufficient_quota", "billing", "pre_consume", "quota",
			"额度", "余额", "欠费", "insufficient balance", "credit"):
			return kindQuota
		case ae.status == http.StatusUnauthorized || hasAny(m, "invalid token", "invalid_api_key", "incorrect api key",
			"invalid api key", "无效的令牌", "令牌已过期", "令牌已被禁用", "令牌状态不可用", "unauthorized"):
			return kindAuth
		case hasAny(m, "model_not_found", "无可用渠道", "no available channel", "does not exist", "模型不存在"):
			return kindModel
		case hasAny(m, "moderation", "safety", "content_policy", "content policy", "content_filter", "content filter",
			"policy violation", "violat", "copyright", "trademark", "blocked by", "rejected by",
			"违规", "敏感", "审核", "版权", "违禁", "不合规", "不当内容", "安全策略",
			// 实测中转站的审核拒绝: HTTP 502 "生成服务拒绝了请求，请检查提示词、参考图或模型参数后重试（积分已退还）"
			"拒绝了请求", "拒绝生成", "检查提示词", "refused"):
			return kindModeration
		case ae.status == http.StatusTooManyRequests || ae.status == http.StatusServiceUnavailable ||
			hasAny(m, "rate limit", "rate_limit", "overloaded", "too many requests", "负载", "饱和", "繁忙", "限流"):
			return kindBusy
		case ae.status >= 500:
			return kindUpstream
		case ae.status == http.StatusNotFound || ae.status == http.StatusMethodNotAllowed:
			return kindConfig
		}
		return kindOther
	}
	var ne net.Error
	switch {
	case errors.Is(err, errNoImage):
		return kindNoImage
	case errors.Is(err, errBadInput):
		return kindBadInput
	case errors.Is(err, errNoEditURL):
		return kindConfig
	case errors.As(err, &ne) && ne.Timeout(), errors.Is(err, context.DeadlineExceeded):
		return kindTimeout
	case errors.As(err, &ne):
		return kindNetwork
	}
	return kindOther
}

// errText 给群友看的失败原因 (调用方补上"这次不算次数")
func errText(err error) string {
	switch classify(err) {
	case kindQuota:
		return "画图接口的额度用完了，已经告诉主人了，充值之前暂时画不了"
	case kindAuth:
		return "画图接口的密钥失效了，已经告诉主人了"
	case kindModel:
		return "画图模型暂时用不了，已经告诉主人了，过会儿再试试"
	case kindModeration:
		return "这个描述或图片没通过审核，可能涉及敏感内容或者版权角色，换个说法试试"
	case kindBusy:
		return "画图接口现在太忙了，过会儿再试试"
	case kindUpstream:
		return "画图接口那边出错了，过会儿再试试"
	case kindTimeout:
		return "画了 5 分钟还没画好，超时了，过会儿再试试"
	case kindNetwork:
		return "连不上画图接口，过会儿再试试"
	case kindNoImage:
		return "接口没有返回图片，可能是内容被拒了，换个说法试试"
	case kindBadInput:
		return "参考图下载不了或者格式不支持，换张图试试"
	case kindConfig:
		if errors.Is(err, errNoEditURL) {
			return errNoEditURL.Error()
		}
		return "画图接口不支持这个操作，需要主人检查一下接口地址"
	case kindOther: // 没见过的错误: 直接给出中转站的报错
		var ae *apiError
		if errors.As(err, &ae) && ae.msg != "" {
			return "画图失败了：" + cutRunes(ae.msg, 80)
		}
	}
	return "画图失败了，过会儿再试试"
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// ownerNote 需要主人处理的问题, 私聊告诉主人; 其余返回空
func ownerNote(k errKind) string {
	switch k {
	case kindQuota:
		return "画图接口额度不足，需要去中转站充值"
	case kindAuth:
		return "画图接口的密钥失效了，需要私聊我重新设置AI画图密钥"
	case kindModel:
		return "画图模型在中转站上没有可用渠道，可能需要换模型名"
	case kindConfig:
		return "画图接口地址可能不对"
	case kindModeration, kindBusy, kindUpstream, kindTimeout, kindNetwork, kindNoImage, kindBadInput, kindOther:
		// 群友换个说法或者过会儿再试就行, 不打扰主人
	}
	return ""
}

const noteInterval = 30 * time.Minute

var (
	notemu sync.Mutex
	noted  = map[errKind]time.Time{}
)

// shouldNote 同一类问题 30 分钟内只提醒主人一次
func shouldNote(k errKind, now time.Time) bool {
	notemu.Lock()
	defer notemu.Unlock()
	if t, ok := noted[k]; ok && now.Sub(t) < noteInterval {
		return false
	}
	noted[k] = now
	return true
}

// notifyOwners 群友画图失败且需要主人处理时, 私聊每个主人
func notifyOwners(ctx *zero.Ctx, k errKind, err error, gid, uid int64) {
	note := ownerNote(k)
	if note == "" || !shouldNote(k, time.Now()) {
		return
	}
	where := "私聊"
	if gid != 0 {
		where = "群 " + strconv.FormatInt(gid, 10)
	}
	text := "【AI画图】" + note + "\n原始错误：" + cutRunes(err.Error(), 200) +
		"\n（" + where + " 里 " + strconv.FormatInt(uid, 10) + " 的请求失败了，次数已退还；同类问题 30 分钟内不再重复提醒）"
	for _, su := range zero.BotConfig.SuperUsers {
		ctx.SendPrivateMessage(su, message.Text(text))
	}
}
