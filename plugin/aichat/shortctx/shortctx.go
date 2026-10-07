// Package shortctx 群聊短期上下文: 最近约 200 条原文 + 12 小时滚动摘要
//
// 参考主流群聊机器人 (MaiBot 的观察窗口 + 聊天总结, 摘要缓冲记忆) 的做法:
//   - 原文窗口: 群里每条消息 (含 bot 自己的发言) 按时间存最近 200~250 条,
//     超过 250 条时一次挪出最早的 50 条去做摘要. 窗口只在末尾增长,
//     请求前缀在两次挪出之间保持不变, 可以命中 DeepSeek 的前缀缓存
//   - 滚动摘要: 挪出的每 50 条由 LLM 概括成一段, 保留 12 小时;
//     段数超过上限时把最早的几段再合并, 让 12 小时的概括始终很短
//   - 重启后首次使用时从 QQ 的聊天记录回填窗口; 摘要存在 sqlite 里
//   - 只在超级用户开启的群生效 (默认关闭)
package shortctx

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	sql "github.com/FloatTech/sqlite"
	"github.com/fumiama/deepinfra"
	"github.com/fumiama/deepinfra/model"
	"github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"

	"github.com/FloatTech/zbputils/chat"
	"github.com/FloatTech/zbputils/control"
	"github.com/FloatTech/zbputils/ctxext"
	"github.com/FloatTech/zbputils/vevent"
)

// BitmapGctx 置 1 表示本群开启群聊上下文 (aichatcfg 存储位, 默认关闭)
const BitmapGctx = 0x200000

const (
	windowMax   = 250   // 原文窗口超过此条数时挪出最早的 evictChunk 条
	evictChunk  = 50    // 每次挪出并概括的条数
	lineLen     = 100   // 每条原文最多字符数
	windowChars = 20000 // 原文部分总字符上限, 极端情况下的保险
	bootCount   = 200   // 重启后回填的条数
	summaryTTL  = 12 * time.Hour
	summaryMax  = 16  // 12 小时内最多保留的摘要段数, 超过则合并最早的 mergeN 段
	mergeN      = 4   // 每次合并的段数
	summaryLen  = 220 // 每段摘要最多字符数
)

// Enabled 本群是否开启群聊上下文
func Enabled(stor ctxext.Storage) bool {
	return stor.GetBool(BitmapGctx)
}

type line struct {
	ts    int64
	uid   int64
	name  string
	text  string
	self  bool
	msgid string
}

type window struct {
	lines  []line
	booted bool
}

type job struct {
	gid   int64
	lines []line
}

var (
	wmu     sync.Mutex
	windows = map[int64]*window{}

	jobs       = make(chan job, 64)
	workeronce sync.Once

	// enabledGroup 挪出原文时判断该群是否开启 (只为开启的群做摘要), 测试时可替换
	enabledGroup = func(gid int64) bool {
		c, ok := control.Lookup("aichatcfg")
		return ok && c.GetData(gid)&BitmapGctx != 0
	}

	// llm 调用大模型, 测试时可替换
	llm = callAC
)

func init() {
	// 记录群里每条消息
	zero.OnMessage(zero.OnlyGroup).FirstPriority().SetBlock(false).Handle(func(ctx *zero.Ctx) {
		txt := render(ctx.Event.Message, ctx.Event.SelfID)
		if txt == "" || ctx.Event.Sender == nil {
			return
		}
		add(ctx.Event.GroupID, line{
			ts:    ctx.Event.Time,
			uid:   ctx.Event.UserID,
			name:  ctx.Event.Sender.Name(),
			text:  txt,
			self:  ctx.Event.UserID == ctx.Event.SelfID,
			msgid: idstr(ctx.Event.MessageID),
		})
	})
	// 记录 bot 自己在群里发出的消息 (由消息或通知触发的发送)
	hook := func(ctx *zero.Ctx) {
		vevent.HookCtxCaller(ctx, vevent.NewAPICallerReturnHook(ctx, func(req zero.APIRequest, rsp zero.APIResponse, err error) {
			if err != nil || rsp.Status != "ok" {
				return
			}
			var txt string
			switch req.Action {
			case "send_group_msg":
				txt = renderParam(req.Params["message"])
			case "send_group_forward_msg":
				txt = "[合并转发]"
			default:
				return
			}
			gid := paramInt(req.Params["group_id"])
			if gid == 0 || txt == "" {
				return
			}
			add(gid, line{ts: time.Now().Unix(), uid: ctx.Event.SelfID, text: txt, self: true, msgid: rsp.Data.Get("message_id").String()})
		}))
	}
	zero.OnMessage().FirstPriority().SetBlock(false).Handle(hook)
	zero.OnNotice().FirstPriority().SetBlock(false).Handle(hook)
}

func idstr(id any) string {
	switch v := id.(type) {
	case int64:
		return strconv.FormatInt(v, 10)
	case string:
		return v
	case message.ID:
		return v.String()
	default:
		return ""
	}
}

func paramInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	case interface{ Int64() (int64, error) }:
		n, _ := x.Int64()
		return n
	default:
		return 0
	}
}

func cutrunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// render 把消息段转成一行文字: 图片等用占位符, @ 渲染成名字 (@bot 自己略去)
func render(msg message.Message, self int64) string {
	sb := strings.Builder{}
	for _, seg := range msg {
		switch seg.Type {
		case "text":
			sb.WriteString(seg.Data["text"])
		case "at":
			if seg.Data["qq"] == strconv.FormatInt(self, 10) {
				continue
			}
			name := strings.TrimPrefix(seg.Data["name"], "@")
			if name == "" {
				name = seg.Data["qq"]
			}
			sb.WriteString("@" + name + " ")
		case "image":
			sb.WriteString("[图片]")
		case "face", "mface", "bface", "sface":
			sb.WriteString("[表情]")
		case "record":
			sb.WriteString("[语音]")
		case "video":
			sb.WriteString("[视频]")
		case "file":
			sb.WriteString("[文件]")
		case "json", "xml", "forward", "markdown":
			sb.WriteString("[卡片]")
		}
	}
	return cutrunes(strings.TrimSpace(sb.String()), lineLen)
}

func renderParam(v any) string {
	switch m := v.(type) {
	case message.Message:
		return render(m, 0)
	case message.Segment:
		return render(message.Message{m}, 0)
	case string:
		return render(message.ParseMessageFromString(m), 0)
	default:
		return ""
	}
}

// add 追加一条原文, 超出上限时挪出最早的一批去做摘要
func add(gid int64, l line) {
	var evicted []line
	wmu.Lock()
	w, ok := windows[gid]
	if !ok {
		w = &window{}
		windows[gid] = w
	}
	w.lines = append(w.lines, l)
	if len(w.lines) > windowMax {
		evicted = append([]line(nil), w.lines[:evictChunk]...)
		w.lines = append([]line(nil), w.lines[evictChunk:]...)
	}
	wmu.Unlock()
	if len(evicted) > 0 && enabledGroup(gid) {
		workeronce.Do(func() { go worker() })
		select {
		case jobs <- job{gid, evicted}:
		default:
			logrus.Warnln("[shortctx] summary queue full, drop", len(evicted), "lines of", gid)
		}
	}
}

// boot 重启后第一次用到某群时, 用 QQ 的聊天记录把窗口补到 bootCount 条
func boot(ctx *zero.Ctx, gid int64) {
	wmu.Lock()
	w, ok := windows[gid]
	if !ok {
		w = &window{}
		windows[gid] = w
	}
	if w.booted {
		wmu.Unlock()
		return
	}
	w.booted = true
	have := len(w.lines)
	wmu.Unlock()
	if have >= bootCount {
		return
	}
	hist := parseHistory(ctx.GetLatestGroupMessageHistory(gid, bootCount, false), ctx.Event.SelfID)
	wmu.Lock()
	defer wmu.Unlock()
	seen := make(map[string]struct{}, len(w.lines))
	for _, l := range w.lines {
		seen[l.msgid] = struct{}{}
	}
	var earliest int64 = 1 << 62
	if len(w.lines) > 0 {
		earliest = w.lines[0].ts
	}
	older := make([]line, 0, len(hist))
	for _, l := range hist {
		if _, ok := seen[l.msgid]; ok || l.ts > earliest {
			continue
		}
		older = append(older, l)
	}
	older = append(older, w.lines...)
	w.lines = older
	if len(w.lines) > windowMax {
		w.lines = append([]line(nil), w.lines[len(w.lines)-windowMax:]...)
	}
	logrus.Infoln("[shortctx] booted", gid, "with", len(older), "history lines")
}

// parseHistory 解析 get_group_msg_history 的返回 (从旧到新)
func parseHistory(data gjson.Result, self int64) []line {
	ms := data.Get("messages").Array()
	out := make([]line, 0, len(ms))
	for _, m := range ms {
		uid := m.Get("sender.user_id").Int()
		name := m.Get("sender.card").String()
		if name == "" {
			name = m.Get("sender.nickname").String()
		}
		txt := render(message.ParseMessage([]byte(m.Get("message").Raw)), self)
		if txt == "" {
			continue
		}
		out = append(out, line{ts: m.Get("time").Int(), uid: uid, name: name, text: txt, self: uid == self, msgid: m.Get("message_id").String()})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ts < out[j].ts })
	return out
}

// cst 北京时间, 不依赖服务器时区
var cst = time.FixedZone("CST", 8*3600)

func clock(ts int64, now time.Time) string {
	t, now := time.Unix(ts, 0).In(cst), now.In(cst)
	if t.Format("0102") == now.Format("0102") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

func (l *line) String(now time.Time) string {
	who := "我"
	if !l.self {
		who = l.name + "(" + strconv.FormatInt(l.uid, 10) + ")"
	}
	return clock(l.ts, now) + " " + who + "：" + l.text
}

// Context 返回本群的群聊上下文 (12 小时摘要 + 最近原文), 不含当前这条消息
func Context(ctx *zero.Ctx, gid int64) string {
	boot(ctx, gid)
	return build(gid, idstr(ctx.Event.MessageID), time.Now())
}

func build(gid int64, curmsgid string, now time.Time) string {
	sb := strings.Builder{}
	if sums := summaries(gid, now); len(sums) > 0 {
		sb.WriteString("【今天早些时候群里聊过】（概括，按时间先后）\n")
		for _, s := range sums {
			sb.WriteString("- ")
			sb.WriteString(clock(s.Start, now))
			sb.WriteString("~")
			sb.WriteString(clock(s.End, now))
			sb.WriteString("：")
			sb.WriteString(s.Text)
			sb.WriteByte('\n')
		}
		sb.WriteByte('\n')
	}
	wmu.Lock()
	var lines []line
	if w, ok := windows[gid]; ok {
		lines = append(lines, w.lines...)
	}
	wmu.Unlock()
	strs := make([]string, 0, len(lines))
	total := 0
	for i := range lines {
		if curmsgid != "" && lines[i].msgid == curmsgid {
			continue
		}
		s := lines[i].String(now)
		strs = append(strs, s)
		total += len(s)
	}
	for total > windowChars*3 && len(strs) > 0 { // len 按字节计, 汉字约 3 字节
		total -= len(strs[0])
		strs = strs[1:]
	}
	if len(strs) > 0 {
		sb.WriteString("【最近的群聊记录】（括号里是QQ号，\"我\"是你自己发的，最下面是最新的）\n")
		for _, s := range strs {
			sb.WriteString(s)
			sb.WriteByte('\n')
		}
	}
	return strings.TrimSpace(sb.String())
}

// ---------- 摘要 ----------

// Summary 一段时间的群聊概括
type Summary struct {
	ID    int64  `db:"id"`
	GID   int64  `db:"gid"`
	Start int64  `db:"t0"` // 避开 start/end 等 SQL 关键字
	End   int64  `db:"t1"`
	Text  string `db:"content"`
}

const tableSummary = "summary"

var (
	dbpath = filepath.Join("data", "aichat", "shortctx.db")
	dbmu   sync.Mutex
	db     sql.Sqlite
	dbinit bool

	idmu   sync.Mutex
	lastid int64
)

func newid() int64 {
	idmu.Lock()
	defer idmu.Unlock()
	id := time.Now().UnixNano()
	if id <= lastid {
		id = lastid + 1
	}
	lastid = id
	return id
}

// opendb 必须持有 dbmu
func opendb() error {
	if dbinit {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dbpath), 0o755); err != nil {
		return err
	}
	db = sql.New(dbpath)
	if err := db.Open(time.Hour); err != nil {
		return err
	}
	if err := db.Create(tableSummary, &Summary{}); err != nil {
		return err
	}
	dbinit = true
	return nil
}

// summaries 本群 12 小时内的摘要, 按时间先后
func summaries(gid int64, now time.Time) []*Summary {
	dbmu.Lock()
	defer dbmu.Unlock()
	if opendb() != nil {
		return nil
	}
	ss, err := sql.FindAll[Summary](&db, tableSummary, "WHERE gid = ? AND t1 >= ? ORDER BY t0", gid, now.Add(-summaryTTL).Unix())
	if err != nil {
		return nil
	}
	return ss
}

func worker() {
	for j := range jobs {
		if chat.AC.Key == "" {
			continue
		}
		if err := summarize(j.gid, j.lines, time.Now()); err != nil {
			logrus.Warnln("[shortctx] summarize err:", err)
		}
	}
}

func callAC(prompt string) (string, error) {
	x := deepinfra.NewAPI(chat.AC.API, string(chat.AC.Key))
	mod, err := chat.AC.Type.Protocol(chat.AC.ModelName, 0.3, 0.9, 512, chat.AC.ReasoningEffort)
	if err != nil {
		return "", err
	}
	return x.Request(mod.User(model.NewContentText(prompt)))
}

func selfname() string {
	if len(zero.BotConfig.NickName) > 0 {
		return zero.BotConfig.NickName[0]
	}
	return "你"
}

const summaryRules = `要求：按时间顺序简洁地记下聊了哪些话题、谁说了什么关键观点、新出现的梗/外号/约定、和我有关的事；提到你自己写"我"，你是真人群友，不要写成机器人、bot或AI；不评价，不要用"这段聊天"之类的开头，不用列表。只输出概括本身。`

// summarize 把挪出窗口的一批原文概括成一段, 并维护 12 小时内的段数
func summarize(gid int64, lines []line, now time.Time) error {
	if len(lines) == 0 {
		return nil
	}
	strs := make([]string, len(lines))
	for i := range lines {
		strs[i] = lines[i].String(now)
	}
	start, end := lines[0].ts, lines[len(lines)-1].ts
	prompt := "你是QQ群里的群友「" + selfname() + "」。下面是群里 " + clock(start, now) + "~" + clock(end, now) +
		" 的聊天记录（括号里是QQ号，\"我\"是你自己的发言）。用不超过150字概括这段时间。\n" + summaryRules +
		"\n\n" + strings.Join(strs, "\n")
	txt, err := llm(prompt)
	if err != nil {
		return err
	}
	txt = cutrunes(txt, summaryLen)
	if txt == "" {
		return nil
	}
	dbmu.Lock()
	defer dbmu.Unlock()
	if err := opendb(); err != nil {
		return err
	}
	if err := db.Insert(tableSummary, &Summary{ID: newid(), GID: gid, Start: start, End: end, Text: txt}); err != nil {
		return err
	}
	logrus.Infoln("[shortctx] summarized", len(lines), "lines of", gid)
	return compact(gid, now)
}

// compact 删掉 12 小时前的摘要; 段数超过上限时把最早的 mergeN 段合并成一段. 必须持有 dbmu
func compact(gid int64, now time.Time) error {
	if err := db.Del(tableSummary, "WHERE gid = ? AND t1 < ?", gid, now.Add(-summaryTTL).Unix()); err != nil {
		return err
	}
	for {
		ss, err := sql.FindAll[Summary](&db, tableSummary, "WHERE gid = ? ORDER BY t0", gid)
		if errors.Is(err, sql.ErrNullResult) || len(ss) <= summaryMax {
			return nil
		}
		if err != nil {
			return err
		}
		old := ss[:mergeN]
		parts := make([]string, len(old))
		for i, s := range old {
			parts[i] = clock(s.Start, now) + "~" + clock(s.End, now) + "：" + s.Text
		}
		prompt := "你是QQ群里的群友「" + selfname() + "」。下面是群里今天几段时间的聊天概括（按时间顺序）。把它们合并成一段不超过200字的概括，保留主要话题、关键人物和梗/约定，去掉重复和琐碎内容。\n" +
			summaryRules + "\n\n" + strings.Join(parts, "\n")
		dbmu.Unlock()
		txt, err := llm(prompt)
		dbmu.Lock()
		if err != nil {
			return err
		}
		txt = cutrunes(txt, summaryLen)
		if txt == "" {
			return nil
		}
		for _, s := range old {
			if err := db.Del(tableSummary, "WHERE id = ?", s.ID); err != nil {
				return err
			}
		}
		if err := db.Insert(tableSummary, &Summary{ID: newid(), GID: gid, Start: old[0].Start, End: old[len(old)-1].End, Text: txt}); err != nil {
			return err
		}
		logrus.Infoln("[shortctx] merged", len(old), "summaries of", gid)
	}
}
