package focus

// 图片输入: 当前消息、被引用消息里的图片/表情, 以及同一个人刚发的图片 (先发图再@问),
// 下载后以 base64 附在最后一条消息里. 文字里用 [图片N] [表情N] 占位, 与附图一一对应.

import (
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fumiama/deepinfra/model"
	"github.com/sirupsen/logrus"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"
)

const (
	maxImages      = 3                // 每次最多附几张图
	maxImageBytes  = 8 << 20          // 单张图片最大字节数
	imageTimeout   = 15 * time.Second // 单张图片下载超时
	recentImageTTL = 2 * time.Minute  // 先发图再@bot 问的有效时间
)

// imagehosts 只下载 QQ 自己的图床, 不让消息里的地址把 bot 引到别处
var imagehosts = []string{".qq.com", ".qq.com.cn", ".qpic.cn", ".gtimg.cn"}

var errImageTooLarge = errors.New("image too large")

var imgclient = &http.Client{
	Timeout: imageTimeout,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !allowedImageURL(req.URL.String()) {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

func allowedImageURL(u string) bool {
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") {
		return false
	}
	h := "." + strings.ToLower(pu.Hostname())
	for _, s := range imagehosts {
		if strings.HasSuffix(h, s) {
			return true
		}
	}
	return false
}

// isImage 图片或带图的商城表情
func isImage(seg message.Segment) bool {
	return seg.Type == "image" || (seg.Type == "mface" && seg.Data["url"] != "")
}

// HasImage 当前消息是否带图
func HasImage(ctx *zero.Ctx) bool {
	for _, seg := range ctx.Event.Message {
		if isImage(seg) {
			return true
		}
	}
	return false
}

type imgref struct {
	label string // 图片1 / 表情2
	url   string
}

// imgset 按出现顺序给图片编号, 超出上限或地址不可用的只留不带编号的占位
type imgset struct {
	refs []imgref
}

// add 返回放进文字里的占位符
func (s *imgset) add(seg message.Segment) string {
	kind := "图片"
	if seg.Type == "mface" || seg.Data["subType"] == "1" || seg.Data["sub_type"] == "1" {
		kind = "表情"
	}
	u := strings.ReplaceAll(seg.Data["url"], "&amp;", "&")
	if len(s.refs) >= maxImages || !allowedImageURL(u) {
		if sm := strings.Trim(seg.Data["summary"], "[]"); seg.Type == "mface" && sm != "" {
			return "[" + kind + "：" + sm + "]"
		}
		return "[" + kind + "]"
	}
	label := kind + strconv.Itoa(len(s.refs)+1)
	s.refs = append(s.refs, imgref{label: label, url: u})
	return "[" + label + "]"
}

func fetchImage(u string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := imgclient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("status " + resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxImageBytes {
		return "", errImageTooLarge
	}
	return model.NewContentImageDataBase64URL(data)
}

// fetch 用于测试替换
var fetch = fetchImage

// LoadImages 并发下载请求里的图片. 下载失败的图在文字里注明看不到, 以免模型编造.
func (r *Request) LoadImages() {
	if len(r.images) == 0 {
		return
	}
	urls := make([]string, len(r.images))
	var wg sync.WaitGroup
	for i, ref := range r.images {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			du, err := fetch(u)
			if err != nil {
				logrus.Infoln("[aichat] focus load image failed:", err)
				return
			}
			urls[i] = du
		}(i, ref.url)
	}
	wg.Wait()
	r.imgparts = r.imgparts[:0]
	failed := make([]string, 0, len(r.images))
	for i, ref := range r.images {
		if urls[i] == "" {
			failed = append(failed, "["+ref.label+"]")
			continue
		}
		r.imgparts = append(r.imgparts, model.NewContentText("["+ref.label+"]"), model.NewContentImageURL(urls[i]))
	}
	r.imgnote = ""
	if len(failed) > 0 {
		r.imgnote = "（" + strings.Join(failed, "") + "没加载出来，你看不到内容）"
	}
}

// HasImages 是否附带了图片
func (r *Request) HasImages() bool {
	return len(r.imgparts) > 0
}

// DropImages 接口不支持图片时去掉图片, 只按文字回答
func (r *Request) DropImages() {
	if len(r.imgparts) == 0 {
		return
	}
	r.imgparts = nil
	r.imgnote = "（图片没加载出来，你看不到内容）"
}

type recentimg struct {
	msgid string
	segs  []message.Segment
	t     time.Time
}

var (
	rimu       sync.Mutex
	recentimgs = map[threadkey]recentimg{}
)

func addrecent(k threadkey, msgid string, segs []message.Segment, now time.Time) {
	rimu.Lock()
	defer rimu.Unlock()
	recentimgs[k] = recentimg{msgid: msgid, segs: segs, t: now}
	if len(recentimgs) > 256 {
		for key, ri := range recentimgs {
			if now.Sub(ri.t) > recentImageTTL {
				delete(recentimgs, key)
			}
		}
	}
}

// takerecent 取出同一个人刚发的图 (不含当前消息本身). 取过就删, 同一张图只附一次.
func takerecent(k threadkey, curmsgid string, now time.Time) []message.Segment {
	rimu.Lock()
	defer rimu.Unlock()
	ri, ok := recentimgs[k]
	if !ok {
		return nil
	}
	delete(recentimgs, k)
	if ri.msgid == curmsgid || now.Sub(ri.t) > recentImageTTL {
		return nil
	}
	return ri.segs
}

func init() {
	// 记下每个人最近一次发的图, 供"先发图再@问"使用
	zero.OnMessage(func(ctx *zero.Ctx) bool {
		return ctx.Event.Sender != nil && HasImage(ctx)
	}).FirstPriority().SetBlock(false).Handle(func(ctx *zero.Ctx) {
		segs := make([]message.Segment, 0, maxImages)
		for _, seg := range ctx.Event.Message {
			if isImage(seg) && len(segs) < maxImages {
				segs = append(segs, seg)
			}
		}
		addrecent(threadkey{gid: groupOf(ctx), uid: ctx.Event.UserID}, msgidstr(ctx.Event.MessageID), segs, time.Now())
	})
}
