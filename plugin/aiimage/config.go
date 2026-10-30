// Package aiimage 提供AI画图功能配置
package aiimage

import (
	"fmt"
	"strings"
	"sync"
	"time"

	sql "github.com/FloatTech/sqlite"
)

// storage 管理画图配置存储
type storage struct {
	sync.RWMutex
	db sql.Sqlite
}

// imageConfig 存储AI画图配置信息
type imageConfig struct {
	ID        int64  `db:"id"`        // 主键ID
	APIKey    string `db:"apiKey"`    // API密钥
	APIURL    string `db:"apiUrl"`    // API地址
	ModelName string `db:"modelName"` // 画图模型名称
}

// usage 每人每天画了几张 (含正在画的, 主人不计)
type usage struct {
	UID   int64 `db:"uid"`
	Day   int64 `db:"day"` // 北京时间 20261030
	Count int64 `db:"count"`
}

var cst = time.FixedZone("CST", 8*3600)

func today(now time.Time) int64 {
	t := now.In(cst)
	return int64(t.Year()*10000 + int(t.Month())*100 + t.Day())
}

// getConfig 获取当前配置
func (sdb *storage) getConfig() imageConfig {
	sdb.RLock()
	defer sdb.RUnlock()
	cfg := imageConfig{}
	_ = sdb.db.Find("config", &cfg, "WHERE id = 1")
	return cfg
}

// setConfig 设置AI画图配置
func (sdb *storage) setConfig(apiKey, apiURL, modelName string) error {
	sdb.Lock()
	defer sdb.Unlock()
	return sdb.db.Insert("config", &imageConfig{
		ID:        1,
		APIKey:    apiKey,
		APIURL:    apiURL,
		ModelName: modelName,
	})
}

// used 今天已经用掉几张 (含正在画的)
func (sdb *storage) used(uid int64, now time.Time) int64 {
	sdb.RLock()
	defer sdb.RUnlock()
	u := usage{}
	if err := sdb.db.Find("usage", &u, "WHERE uid = ?", uid); err != nil || u.Day != today(now) {
		return 0
	}
	return u.Count
}

// reserve 接单时先占一个名额, 这样同时发很多条也超不过上限. 返回占用的日期和占用后还剩几张
func (sdb *storage) reserve(uid int64, now time.Time, limit int64) (day, left int64, ok bool) {
	sdb.Lock()
	defer sdb.Unlock()
	day = today(now)
	u := usage{}
	if err := sdb.db.Find("usage", &u, "WHERE uid = ?", uid); err != nil || u.Day != day {
		u = usage{UID: uid, Day: day}
	}
	if u.Count >= limit {
		return day, 0, false
	}
	u.Count++
	if err := sdb.db.Insert("usage", &u); err != nil {
		return day, 0, false
	}
	return day, limit - u.Count, true
}

// refund 画失败了把名额还回去 (已经跨天就不用还了)
func (sdb *storage) refund(uid, day int64) {
	sdb.Lock()
	defer sdb.Unlock()
	u := usage{}
	if err := sdb.db.Find("usage", &u, "WHERE uid = ?", uid); err != nil || u.Day != day || u.Count <= 0 {
		return
	}
	u.Count--
	_ = sdb.db.Insert("usage", &u)
}

// maskKey 只露出密钥首尾几位
func maskKey(k string) string {
	if k == "" {
		return "（未设置）"
	}
	if len(k) <= 10 {
		return strings.Repeat("*", len(k))
	}
	return k[:4] + strings.Repeat("*", 8) + k[len(k)-4:]
}

// PrintConfig 返回格式化后的配置信息
func (sdb *storage) PrintConfig() string {
	cfg := sdb.getConfig()
	var builder strings.Builder
	builder.WriteString("当前AI画图配置:\n")
	builder.WriteString(fmt.Sprintf("• 密钥: %s\n", maskKey(cfg.APIKey)))
	builder.WriteString(fmt.Sprintf("• 接口地址: %s\n", cfg.APIURL))
	builder.WriteString(fmt.Sprintf("• 模型名: %s\n", cfg.ModelName))
	builder.WriteString(fmt.Sprintf("• 每人每天: %d 张（主人不限）\n", dailyLimit))
	builder.WriteString(fmt.Sprintf("• 同时最多画: %d 张\n", maxInflight))
	return builder.String()
}
