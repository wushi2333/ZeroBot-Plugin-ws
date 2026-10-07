// Package focus AI聊天专注模式: 回复只围绕触发的那句话展开
//
// 权重从高到低:
//  1. 当前消息 (放在请求最后, 单独标注)
//  2. 被引用的消息 (当前消息回复了某条消息时)
//  3. 该用户与 bot 的历史对话 (按 群+用户 单独记录, 作为真正的多轮对话)
//  4. 群聊背景 (其他人最近几条发言, 明确标注为低权重参考)
package focus

import (
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fumiama/deepinfra"
	"github.com/fumiama/deepinfra/model"
	zero "github.com/wdvxdr1123/ZeroBot"

	"github.com/FloatTech/zbputils/ctxext"
)

// BitmapNfcs 置 1 表示本群不使用专注模式 (aichatcfg 存储位)
const BitmapNfcs = 0x080000

const (
	bgSize      = 6                // 群聊背景条数
	bgItemLen   = 80               // 每条背景最多字符数
	quoteLen    = 300              // 被引用消息最多字符数
	msgLen      = 1000             // 当前消息最多字符数
	threadTurns = 12               // 用户线程最多保留消息数 (6 轮)
	threadTTL   = 30 * time.Minute // 用户线程闲置多久后清空
	passToken   = "<pass>"         // 非@触发时模型可以选择不回复
)

const rules = `【回复规则】
1. 你只需要回应「当前消息」这一句话。
2. 「群聊背景」是群里其他人最近的发言，权重很低，只有在当前消息明显需要时才参考；与当前消息无关就完全忽略，不要复述、总结、评论背景内容，也不要回应背景里的其他人。
3. 你和当前用户之前的对话是主要上下文，可以延续。
4. 「引用的消息」是当前用户回复的那条消息，回答时要结合它。
5. 像群友一样简短自然地回复，只输出一行纯文本，不要带【】或用户名前缀。`

const passRule = "\n6. 这次不是有人@你，而是你在旁听群聊。如果当前消息与你无关或不值得接话，只输出 " + passToken + "。"

// Uses 本群是否使用专注模式 (默认使用)
func Uses(stor ctxext.Storage) bool {
	return !stor.GetBool(BitmapNfcs)
}

type bgline struct {
	msgid string
	uid   int64
	name  string
	text  string
}

type turn struct {
	isbot bool
	text  string
}

type thread struct {
	turns []turn
	last  time.Time
}

type threadkey struct {
	gid, uid int64
}

var (
	bgmu    sync.Mutex
	bglines = map[int64][]bgline{}

	thmu    sync.Mutex
	threads = map[threadkey]*thread{}
)

func init() {
	// 记录所有群/私聊的纯文本消息作为背景, 每群只保留最近几条
	zero.OnMessage(func(ctx *zero.Ctx) bool {
		return ctx.Event.Sender != nil && strings.TrimSpace(ctx.ExtractPlainText()) != ""
	}).FirstPriority().SetBlock(false).Handle(func(ctx *zero.Ctx) {
		addbg(groupOf(ctx), bgline{
			msgid: msgidstr(ctx.Event.MessageID),
			uid:   ctx.Event.UserID,
			name:  ctx.Event.Sender.Name(),
			text:  ctx.ExtractPlainText(),
		})
	})
}

func groupOf(ctx *zero.Ctx) int64 {
	if ctx.Event.GroupID == 0 {
		return -ctx.Event.UserID
	}
	return ctx.Event.GroupID
}

func msgidstr(id any) string {
	switch v := id.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		return v
	default:
		return ""
	}
}

func addbg(gid int64, l bgline) {
	l.text = cutrunes(oneline(l.text), bgItemLen)
	bgmu.Lock()
	defer bgmu.Unlock()
	bglines[gid] = append(bglines[gid], l)
	// 多留一条, 取用时要排除当前消息本身
	if lines := bglines[gid]; len(lines) > bgSize+1 {
		bglines[gid] = lines[len(lines)-bgSize-1:]
	}
}

// getbg 取最近的背景, 排除当前消息
func getbg(gid int64, curmsgid string) []bgline {
	bgmu.Lock()
	defer bgmu.Unlock()
	lines := bglines[gid]
	out := make([]bgline, 0, len(lines))
	for _, l := range lines {
		if curmsgid != "" && l.msgid == curmsgid {
			continue
		}
		out = append(out, l)
	}
	if len(out) > bgSize {
		out = out[len(out)-bgSize:]
	}
	return out
}

func getthread(k threadkey, now time.Time) []turn {
	thmu.Lock()
	defer thmu.Unlock()
	t, ok := threads[k]
	if !ok {
		return nil
	}
	if now.Sub(t.last) > threadTTL {
		delete(threads, k)
		return nil
	}
	return append([]turn(nil), t.turns...)
}

func addthread(k threadkey, now time.Time, ts ...turn) {
	thmu.Lock()
	defer thmu.Unlock()
	t, ok := threads[k]
	if !ok || now.Sub(t.last) > threadTTL {
		t = &thread{}
		threads[k] = t
	}
	t.turns = append(t.turns, ts...)
	if len(t.turns) > threadTurns {
		t.turns = t.turns[len(t.turns)-threadTurns:]
	}
	t.last = now
	// 顺手清理过期线程, 防止长期运行时无限增长
	if len(threads) > 512 {
		for key, th := range threads {
			if now.Sub(th.last) > threadTTL {
				delete(threads, key)
			}
		}
	}
}

// Reset 清空专注模式的背景与用户线程 (grps 为空时清空全部)
func Reset(grps ...int64) {
	bgmu.Lock()
	thmu.Lock()
	defer bgmu.Unlock()
	defer thmu.Unlock()
	if len(grps) == 0 {
		bglines = map[int64][]bgline{}
		threads = map[threadkey]*thread{}
		return
	}
	for _, g := range grps {
		delete(bglines, g)
		for k := range threads {
			if k.gid == g {
				delete(threads, k)
			}
		}
	}
}

func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func cutrunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func speaker(name string, uid int64) string {
	return name + "(" + strconv.FormatInt(uid, 10) + ")"
}

// Persona 生成专注模式的系统提示词
func Persona(nick, sex, char string, isatme bool) string {
	sb := strings.Builder{}
	sb.WriteString("你是QQ群里的成员「")
	sb.WriteString(nick)
	sb.WriteString("」")
	if sex != "" {
		sb.WriteString("，性别")
		sb.WriteString(sex)
	}
	sb.WriteString("。\n")
	if char = strings.TrimSpace(char); char != "" {
		sb.WriteString("【人设】\n")
		sb.WriteString(char)
		sb.WriteString("\n")
	}
	sb.WriteString(rules)
	if !isatme {
		sb.WriteString(passRule)
	}
	return sb.String()
}

// Request 一次专注模式请求
type Request struct {
	persona  string
	thread   []turn
	bg       []bgline
	quote    string // 已格式化的引用, 可为空
	sender   string // 已格式化的发言人
	text     string
	isatme   bool
	nosystem bool // 接口不支持 system 时把人设并入第一条 user

	key threadkey
	now time.Time
}

// NewRequest 从当前消息组装请求, 当前消息没有文字时返回 nil
func NewRequest(ctx *zero.Ctx, persona string, nosystem bool) *Request {
	text := strings.TrimSpace(ctx.ExtractPlainText())
	if text == "" || ctx.Event.Sender == nil {
		return nil
	}
	gid := groupOf(ctx)
	now := time.Now()
	k := threadkey{gid: gid, uid: ctx.Event.UserID}
	return &Request{
		persona:  persona,
		thread:   getthread(k, now),
		bg:       getbg(gid, msgidstr(ctx.Event.MessageID)),
		quote:    quoteOf(ctx),
		sender:   speaker(ctx.Event.Sender.Name(), ctx.Event.UserID),
		text:     cutrunes(text, msgLen),
		isatme:   ctx.Event.IsToMe,
		nosystem: nosystem,
		key:      k,
		now:      now,
	}
}

// finalUser 生成最后一条 user 消息: 背景在前, 当前消息压轴
func (r *Request) finalUser() string {
	sb := strings.Builder{}
	if len(r.bg) > 0 {
		sb.WriteString("【群聊背景（低权重，无关请忽略，不要回应）】\n")
		for _, l := range r.bg {
			sb.WriteString(speaker(l.name, l.uid))
			sb.WriteString("：")
			sb.WriteString(l.text)
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	if r.quote != "" {
		sb.WriteString("【引用的消息】\n")
		sb.WriteString(r.quote)
		sb.WriteString("\n\n")
	}
	sb.WriteString("【当前消息】")
	if r.isatme {
		sb.WriteString("（@你）")
	}
	sb.WriteByte('\n')
	sb.WriteString(r.sender)
	sb.WriteString("：")
	sb.WriteString(r.text)
	return sb.String()
}

// Modelize 按 system -> 用户线程 -> 最终 user 的顺序填充协议
func (r *Request) Modelize(p model.Protocol) deepinfra.Model {
	m := p
	prefix := ""
	if r.nosystem {
		prefix = r.persona + "\n\n"
	} else {
		m = m.System(r.persona)
	}
	for _, t := range r.thread {
		if t.isbot {
			m = m.Assistant(model.NewContentText(t.text))
			continue
		}
		m = m.User(model.NewContentText(prefix + t.text))
		prefix = ""
	}
	return m.User(model.NewContentText(prefix + r.finalUser()))
}

// Done 把本轮问答记入该用户的线程
func (r *Request) Done(reply string) {
	usr := r.text
	if r.quote != "" {
		usr = "（引用 " + r.quote + "）" + usr
	}
	addthread(r.key, r.now, turn{text: usr}, turn{isbot: true, text: reply})
}

// IsPass 模型选择不回复
func IsPass(s string) bool {
	return strings.Contains(strings.ToLower(s), passToken)
}

// quoteOf 当前消息若回复了某条消息, 返回格式化的被引用内容
func quoteOf(ctx *zero.Ctx) string {
	for _, elem := range ctx.Event.Message {
		if elem.Type != "reply" {
			continue
		}
		id, err := strconv.ParseInt(elem.Data["id"], 10, 64)
		if err != nil {
			return ""
		}
		msg := ctx.GetMessage(id, true)
		txt := strings.TrimSpace(msg.Elements.ExtractPlainText())
		if txt == "" {
			return ""
		}
		who := "某人"
		if msg.Sender != nil {
			if msg.Sender.ID == ctx.Event.SelfID {
				who = "你自己"
			} else {
				who = speaker(msg.Sender.Name(), msg.Sender.ID)
			}
		}
		return who + "：" + cutrunes(oneline(txt), quoteLen)
	}
	return ""
}
