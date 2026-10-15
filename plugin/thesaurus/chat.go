// Package thesaurus 修改过的单纯回复插件, 仅@触发
package thesaurus

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"

	"github.com/FloatTech/AnimeAPI/kimoi"
	ctrl "github.com/FloatTech/zbpctrl"
	"github.com/FloatTech/zbputils/control"
)

// chatTimeout kimoi.Chat 用的是没有超时的 http.Post, 在线服务卡住时不能让整条消息的处理跟着卡住
const chatTimeout = 15 * time.Second

var errChatTimeout = errors.New("kimoi chat timeout")

func init() {
	engine := control.AutoRegister(&ctrl.Options[*zero.Ctx]{
		DisableOnDefault: false,
		Brief:            "词典匹配回复, 仅@触发",
		Help: "- @bot 随便说点什么\n" +
			"- 不想要的回复写进 data/Chat/kimoi_block.json (JSON 字符串数组, 改完自动生效), 被屏蔽的回复不发, 交给 AI 回答",
		PublicDataFolder: "Chat",
	})
	blocked := &blocklist{path: engine.DataFolder() + "kimoi_block.json"}
	engine.OnMessage(zero.OnlyToMe, canmatch()).
		SetBlock(false).Handle(func(ctx *zero.Ctx) {
		msg := ctx.ExtractPlainText()
		r, err := chat(msg)
		if err == nil {
			c := 0
			for r.Confidence < 0.2 && c < 3 {
				r, err = chat(msg)
				if err != nil {
					return
				}
				c++
			}
			if r.Confidence < 0.2 {
				return
			}
			if blocked.has(r.Reply) { // 不阻断, 让后面的 AI 来回答
				logrus.Infoln("[thesaurus] kimoi reply blocked:", r.Reply)
				return
			}
			logrus.Infoln("[thesaurus] kimoi:", msg, "=>", r.Reply)
			ctx.Block()
			ctx.SendChain(message.Text(r.Reply))
		}
	})
}

func canmatch() zero.Rule {
	return func(ctx *zero.Ctx) bool {
		if zero.HasPicture(ctx) {
			return false
		}
		return ctx.ExtractPlainText() != ""
	}
}

// chat 带超时的 kimoi.Chat. 超时后放弃等待 (请求所在的协程会在连接断开后自行结束)
func chat(msg string) (kimoi.Response, error) {
	type result struct {
		r   kimoi.Response
		err error
	}
	ch := make(chan result, 1)
	go func() {
		r, err := kimoi.Chat(msg)
		ch <- result{r, err}
	}()
	select {
	case x := <-ch:
		return x.r, x.err
	case <-time.After(chatTimeout):
		return kimoi.Response{}, errChatTimeout
	}
}

// blocklist 屏蔽的 kimoi 回复, 文件改动后自动重新加载
type blocklist struct {
	mu   sync.Mutex
	path string
	mod  time.Time
	set  map[string]struct{}
}

func norm(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// has 回复是否被屏蔽 (忽略首尾和连续空白的差异)
func (b *blocklist) has(reply string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	fi, err := os.Stat(b.path)
	switch {
	case err != nil:
		b.set, b.mod = nil, time.Time{}
	case !fi.ModTime().Equal(b.mod):
		b.load(fi.ModTime())
	}
	_, ok := b.set[norm(reply)]
	return ok
}

// load 读取失败或格式不对时保留原来的列表, 免得改坏文件后屏蔽全部失效
func (b *blocklist) load(mod time.Time) {
	data, err := os.ReadFile(b.path)
	if err != nil {
		logrus.Warnln("[thesaurus] read", b.path, "err:", err)
		return
	}
	var list []string
	if err := json.Unmarshal(data, &list); err != nil {
		logrus.Warnln("[thesaurus] parse", b.path, "err:", err)
		b.mod = mod // 同一份坏文件不反复报错
		return
	}
	set := make(map[string]struct{}, len(list))
	for _, s := range list {
		if s = norm(s); s != "" {
			set[s] = struct{}{}
		}
	}
	b.set, b.mod = set, mod
	logrus.Infoln("[thesaurus] loaded", len(set), "blocked kimoi replies")
}
