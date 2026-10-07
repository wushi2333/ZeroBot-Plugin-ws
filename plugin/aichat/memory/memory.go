// Package memory 椛椛的日记本: 精简版长期记忆
//
// 设计参考 VCPToolBox 的日记本 (AI 自己写日记, 按需浮现而非全部塞进上下文),
// MaiBot 的人物印象与遗忘/回忆强化, mem0 的 LLM 事实抽取. 为适配小内存服务器且
// DeepSeek 没有 embedding 接口, 检索用中文二字词重叠度代替向量, 存储用 sqlite.
//
//   - 人物印象: 每群每人一条 (≤200 字), 该用户说话时总会带上
//   - 日记条目: 值得长期记住的事实/事件, 按相关度×重要度×时间衰减只浮现最相关的几条,
//     被回忆起会加强 (hits)
//   - 写日记: 一段对话闲置后由 LLM 更新印象并抽取新事实 (相似内容合并);
//     启用记忆的群每隔一段时间总结一次群聊里的梗和事件
//   - 遗忘: 每群条目有上限, 按重要度/被回忆次数/闲置时间淘汰
package memory

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	sql "github.com/FloatTech/sqlite"
	"github.com/sirupsen/logrus"

	"github.com/FloatTech/zbputils/ctxext"
)

// BitmapNmem 置 1 表示本群不使用记忆 (aichatcfg 存储位)
const BitmapNmem = 0x100000

const (
	tableEntry   = "entry"
	tableProfile = "profile"

	recallTop      = 3    // 每次最多浮现几条日记
	recallMinScore = 0.12 // 低于此分数不浮现
	mergeSim       = 0.55 // 新事实与旧条目相似度超过此值视为同一件事, 合并
	entryLen       = 60   // 日记条目最长字符数
	profileLen     = 200  // 印象最长字符数
	groupCap       = 200  // 每群最多保留条目
)

// Enabled 本群是否使用记忆 (默认使用, 仅在专注模式下生效)
func Enabled(stor ctxext.Storage) bool {
	return !stor.GetBool(BitmapNmem)
}

// Entry 一条日记
type Entry struct {
	ID         int64  `db:"id"`
	GID        int64  `db:"gid"`
	UID        int64  `db:"uid"` // 相关的人, 0 表示群公共
	Content    string `db:"content"`
	Importance int64  `db:"importance"` // 1~3
	Created    int64  `db:"created"`
	LastUsed   int64  `db:"lastused"`
	Hits       int64  `db:"hits"`
}

// Profile 对某人的印象
type Profile struct {
	Key     string `db:"pkey"` // gid:uid
	GID     int64  `db:"gid"`
	UID     int64  `db:"uid"`
	Name    string `db:"name"`
	Text    string `db:"text"`
	Updated int64  `db:"updated"`
}

var (
	dbpath = filepath.Join("data", "aichat", "memory.db")

	dbmu   sync.Mutex
	db     sql.Sqlite
	dbinit bool
)

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
	if err := db.Create(tableEntry, &Entry{}); err != nil {
		return err
	}
	if err := db.Create(tableProfile, &Profile{}); err != nil {
		return err
	}
	dbinit = true
	return nil
}

func profilekey(gid, uid int64) string {
	return strconv.FormatInt(gid, 10) + ":" + strconv.FormatInt(uid, 10)
}

// entriesOf 必须持有 dbmu
func entriesOf(gid int64) ([]*Entry, error) {
	es, err := sql.FindAll[Entry](&db, tableEntry, "WHERE gid = ?", gid)
	if errors.Is(err, sql.ErrNullResult) {
		return nil, nil
	}
	return es, err
}

// profileOf 必须持有 dbmu
func profileOf(gid, uid int64) (Profile, bool) {
	var p Profile
	err := db.Find(tableProfile, &p, "WHERE pkey = ?", profilekey(gid, uid))
	return p, err == nil && p.Text != ""
}

// ---------- 相似度: 中文二字词 (bigram) 集合的余弦 ----------

// qqtag 匹配各种写法的 QQ 号标注: (123) （QQ 123） (QQ号：123) (qq123)
const qqtag = `[(（]\s*(?:qq)?\s*号?\s*[:：]?\s*\d{4,}\s*[)）]`

var (
	// subjre 条目开头的 "名字(QQ号)" 主语
	subjre = regexp.MustCompile(`(?i)^[^\n]{0,20}?` + qqtag)
	// qqre 其余位置的 (QQ号), 以及不带括号的 "QQ 123"
	qqre = regexp.MustCompile(`(?i)` + qqtag + `|qq\s*号?\s*[:：]?\s*\d{4,}`)
)

// core 去掉人名与 QQ 号, 只留内容本身.
// 否则同一个人的两件无关的事会因为共享 "吃(1366348913)" 这串字符而显得很像, 被错误合并.
func core(s string) string {
	return qqre.ReplaceAllString(subjre.ReplaceAllString(s, ""), "")
}

func normalize(s string) []rune {
	rs := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			rs = append(rs, r)
		}
	}
	return rs
}

func grams(s string) map[string]struct{} {
	rs := normalize(core(s))
	m := make(map[string]struct{}, len(rs))
	if len(rs) == 1 {
		m[string(rs)] = struct{}{}
		return m
	}
	for i := 0; i+1 < len(rs); i++ {
		m[string(rs[i:i+2])] = struct{}{}
	}
	return m
}

func similarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	n := 0
	for g := range a {
		if _, ok := b[g]; ok {
			n++
		}
	}
	return float64(n) / math.Sqrt(float64(len(a))*float64(len(b)))
}

// Sim 两段文本的相似度 0~1
func Sim(a, b string) float64 {
	return similarity(grams(a), grams(b))
}

func days(now, ts int64) float64 {
	if ts <= 0 || now <= ts {
		return 0
	}
	return float64(now-ts) / 86400
}

// recallScore 相关度 × 重要度 × 时间衰减 (越久没被想起越淡)
func recallScore(sim float64, e *Entry, uid, now int64) float64 {
	s := sim * (1 + 0.25*float64(e.Importance-1)) / (1 + days(now, e.LastUsed)/60)
	if uid != 0 && e.UID == uid {
		s *= 1.2
	}
	return s
}

// keepScore 遗忘时的保留分: 重要、常被想起、最近用过的留下
func keepScore(e *Entry, now int64) float64 {
	return float64(e.Importance) + 0.5*math.Log1p(float64(e.Hits)) - days(now, e.LastUsed)/30
}

func cutrunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// cutsentence 超长时尽量在句末/逗号处截断, 避免半句话
func cutsentence(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	cut := rs[:n]
	for i := len(cut) - 1; i >= n/2; i-- {
		switch cut[i] {
		case '。', '！', '？', '；', '!', '?', ';':
			return string(cut[:i+1])
		}
	}
	for i := len(cut) - 1; i >= n/2; i-- {
		switch cut[i] {
		case '，', ',', '、':
			return string(cut[:i])
		}
	}
	return string(cut)
}

func ago(now, ts int64) string {
	d := days(now, ts)
	switch {
	case d < 1:
		return "今天"
	case d < 2:
		return "昨天"
	case d < 30:
		return strconv.Itoa(int(d)) + "天前"
	default:
		return strconv.Itoa(int(d/30)) + "个月前"
	}
}

// Recall 一次回忆的结果
type Recall struct {
	Profile string   // 对当前用户的印象, 可为空
	Items   []string // 浮现的日记, 已带时间
}

// Get 回忆与当前消息相关的内容, 并强化被想起的条目
func Get(gid, uid int64, text string) (rc Recall) {
	markActive(gid)
	now := time.Now().Unix()
	dbmu.Lock()
	defer dbmu.Unlock()
	if err := opendb(); err != nil {
		logrus.Warnln("[memory] open db err:", err)
		return
	}
	if p, ok := profileOf(gid, uid); ok {
		rc.Profile = p.Text
	}
	es, err := entriesOf(gid)
	if err != nil {
		logrus.Warnln("[memory] load entries err:", err)
		return
	}
	q := grams(text)
	type scored struct {
		e *Entry
		s float64
	}
	cand := make([]scored, 0, len(es))
	for _, e := range es {
		s := recallScore(similarity(q, grams(e.Content)), e, uid, now)
		if s >= recallMinScore {
			cand = append(cand, scored{e, s})
		}
	}
	sort.Slice(cand, func(i, j int) bool { return cand[i].s > cand[j].s })
	if len(cand) > recallTop {
		cand = cand[:recallTop]
	}
	for _, c := range cand {
		rc.Items = append(rc.Items, "（"+ago(now, c.e.Created)+"）"+c.e.Content)
		_, err := db.Exec("UPDATE "+tableEntry+" SET lastused = ?, hits = hits + 1 WHERE id = ?", now, c.e.ID)
		if err != nil {
			logrus.Warnln("[memory] reinforce err:", err)
		}
	}
	return
}

// ---------- 写入 ----------

// remember 写入一条新事实, 与相似旧条目合并. 必须持有 dbmu
func remember(gid, uid int64, content string, importance int64, now int64, es []*Entry) ([]*Entry, error) {
	content = cutsentence(fixSelf(content), entryLen)
	if content == "" || rejected(content) {
		return es, nil
	}
	if importance < 1 {
		importance = 1
	} else if importance > 3 {
		importance = 3
	}
	g := grams(content)
	var best *Entry
	bestsim := 0.0
	for _, e := range es {
		if s := similarity(g, grams(e.Content)); s > bestsim {
			best, bestsim = e, s
		}
	}
	if best != nil && bestsim >= mergeSim {
		best.Content = content
		if importance > best.Importance {
			best.Importance = importance
		}
		if uid != 0 {
			best.UID = uid
		}
		best.LastUsed = now
		best.Hits++
		return es, db.Insert(tableEntry, best)
	}
	e := &Entry{ID: newid(), GID: gid, UID: uid, Content: content, Importance: importance, Created: now, LastUsed: now}
	return append(es, e), db.Insert(tableEntry, e)
}

var (
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

// setProfile 必须持有 dbmu
func setProfile(gid, uid int64, name, text string, now int64) error {
	text = cutsentence(fixSelf(text), profileLen)
	if text == "" || rejected(text) {
		return nil
	}
	return db.Insert(tableProfile, &Profile{Key: profilekey(gid, uid), GID: gid, UID: uid, Name: name, Text: text, Updated: now})
}

// prune 遗忘超出上限的条目. 必须持有 dbmu
func prune(gid int64, now int64) error {
	es, err := entriesOf(gid)
	if err != nil || len(es) <= groupCap {
		return err
	}
	sort.Slice(es, func(i, j int) bool { return keepScore(es[i], now) > keepScore(es[j], now) })
	for _, e := range es[groupCap:] {
		if err := db.Del(tableEntry, "WHERE id = ?", e.ID); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 查看 / 删除 (命令用) ----------

// About 某人在本群的印象与日记 (最新的在前)
func About(gid, uid int64, limit int) (profile string, items []string) {
	now := time.Now().Unix()
	dbmu.Lock()
	defer dbmu.Unlock()
	if opendb() != nil {
		return
	}
	if p, ok := profileOf(gid, uid); ok {
		profile = p.Text
	}
	es, _ := entriesOf(gid)
	sort.Slice(es, func(i, j int) bool { return es[i].Created > es[j].Created })
	for _, e := range es {
		if e.UID == uid && len(items) < limit {
			items = append(items, "（"+ago(now, e.Created)+"）"+e.Content)
		}
	}
	return
}

// Group 本群的日记 (最新的在前) 与印象数量
func Group(gid int64, limit int) (items []string, total, profiles int) {
	now := time.Now().Unix()
	dbmu.Lock()
	defer dbmu.Unlock()
	if opendb() != nil {
		return
	}
	es, _ := entriesOf(gid)
	total = len(es)
	sort.Slice(es, func(i, j int) bool { return es[i].Created > es[j].Created })
	for _, e := range es {
		if len(items) >= limit {
			break
		}
		items = append(items, "（"+ago(now, e.Created)+"）"+e.Content)
	}
	ps, _ := sql.FindAll[Profile](&db, tableProfile, "WHERE gid = ?", gid)
	profiles = len(ps)
	return
}

// Forget 删除某人在本群的印象与相关日记, uid 为 0 时清空整个群
func Forget(gid, uid int64) error {
	forgetPending(gid, uid)
	dbmu.Lock()
	defer dbmu.Unlock()
	if err := opendb(); err != nil {
		return err
	}
	if uid == 0 {
		if err := db.Del(tableEntry, "WHERE gid = ?", gid); err != nil {
			return err
		}
		return db.Del(tableProfile, "WHERE gid = ?", gid)
	}
	if err := db.Del(tableEntry, "WHERE gid = ? AND uid = ?", gid, uid); err != nil {
		return err
	}
	return db.Del(tableProfile, "WHERE pkey = ?", profilekey(gid, uid))
}
