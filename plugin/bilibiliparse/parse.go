// Package bilibiliparse bilibili卡片解析
package bilibiliparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"strconv"

	bz "github.com/FloatTech/AnimeAPI/bilibili"
	"github.com/FloatTech/floatbox/file"
	"github.com/FloatTech/floatbox/web"
	ctrl "github.com/FloatTech/zbpctrl"
	"github.com/FloatTech/zbputils/control"
	"github.com/FloatTech/zbputils/ctxext"
	"github.com/pkg/errors"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"
)

const (
	enableVideoSummary   = int64(0x10)
	disableVideoSummary  = ^enableVideoSummary
	enableVideoDownload  = int64(0x20)
	disableVideoDownload = ^enableVideoDownload
	bilibiliparseReferer = "https://www.bilibili.com"
	ua                   = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/92.0.4515.107 Safari/537.36"
)

var (
	limit            = ctxext.NewLimiterManager(time.Second*10, 1)
	searchVideo      = `bilibili.com\\?/video\\?/(?:av(\d+)|([bB][vV][0-9a-zA-Z]+))`
	searchDynamic    = `(t.bilibili.com|m.bilibili.com\\?/dynamic)\\?/(\d+)`
	searchArticle    = `bilibili.com\\?/read\\?/(?:cv|mobile\\?/)(\d+)`
	searchLiveRoom   = `live.bilibili.com\\?/(\d+)`
	searchVideoRe    = regexp.MustCompile(searchVideo)
	searchDynamicRe  = regexp.MustCompile(searchDynamic)
	searchArticleRe  = regexp.MustCompile(searchArticle)
	searchLiveRoomRe = regexp.MustCompile(searchLiveRoom)
	cachePath        string
	cfg              = bz.NewCookieConfig("data/Bilibili/config.json")
)

// 插件主体
func init() {
	en := control.AutoRegister(&ctrl.Options[*zero.Ctx]{
		DisableOnDefault: false,
		Brief:            "b站链接解析",
		Help:             "例:- t.bilibili.com/642277677329285174\n- bilibili.com/read/cv17134450\n- bilibili.com/video/BV13B4y1x7pS\n- live.bilibili.com/22603245 ",
	})
	cachePath = en.DataFolder() + "cache/"
	_ = os.RemoveAll(cachePath)
	_ = os.MkdirAll(cachePath, 0755)
	en.OnRegex(`((b23|acg).tv|bili2233.cn)\\?/[0-9a-zA-Z]+`).SetBlock(true).Limit(limit.LimitByGroup).
		Handle(func(ctx *zero.Ctx) {
			u := ctx.State["regex_matched"].([]string)[0]
			u = strings.ReplaceAll(u, "\\", "")
			realurl, err := bz.GetRealURL("https://" + u)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: ", err))
				return
			}
			switch {
			case searchVideoRe.MatchString(realurl):
				ctx.State["regex_matched"] = searchVideoRe.FindStringSubmatch(realurl)
				handleVideo(ctx)
			case searchDynamicRe.MatchString(realurl):
				ctx.State["regex_matched"] = searchDynamicRe.FindStringSubmatch(realurl)
				handleDynamic(ctx)
			case searchArticleRe.MatchString(realurl):
				ctx.State["regex_matched"] = searchArticleRe.FindStringSubmatch(realurl)
				handleArticle(ctx)
			case searchLiveRoomRe.MatchString(realurl):
				ctx.State["regex_matched"] = searchLiveRoomRe.FindStringSubmatch(realurl)
				handleLive(ctx)
			}
		})
	en.OnRegex(`^(开启|打开|启用|关闭|关掉|禁用)视频总结$`, zero.AdminPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			gid := ctx.Event.GroupID
			if gid <= 0 {
				// 个人用户设为负数
				gid = -ctx.Event.UserID
			}
			option := ctx.State["regex_matched"].([]string)[1]
			c, ok := ctx.State["manager"].(*ctrl.Control[*zero.Ctx])
			if !ok {
				ctx.SendChain(message.Text("找不到服务!"))
				return
			}
			data := c.GetData(ctx.Event.GroupID)
			switch option {
			case "开启", "打开", "启用":
				data |= enableVideoSummary
			case "关闭", "关掉", "禁用":
				data &= disableVideoSummary
			default:
				return
			}
			err := c.SetData(gid, data)
			if err != nil {
				ctx.SendChain(message.Text("出错啦: ", err))
				return
			}
			ctx.SendChain(message.Text("已", option, "视频总结"))
		})
	en.OnRegex(`^(开启|打开|启用|关闭|关掉|禁用)视频上传$`, zero.AdminPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			gid := ctx.Event.GroupID
			if gid <= 0 {
				// 个人用户设为负数
				gid = -ctx.Event.UserID
			}
			option := ctx.State["regex_matched"].([]string)[1]
			c, ok := ctx.State["manager"].(*ctrl.Control[*zero.Ctx])
			if !ok {
				ctx.SendChain(message.Text("找不到服务!"))
				return
			}
			data := c.GetData(ctx.Event.GroupID)
			switch option {
			case "开启", "打开", "启用":
				data |= enableVideoDownload
			case "关闭", "关掉", "禁用":
				data &= disableVideoDownload
			default:
				return
			}
			err := c.SetData(gid, data)
			if err != nil {
				ctx.SendChain(message.Text("出错啦: ", err))
				return
			}
			ctx.SendChain(message.Text("已", option, "视频上传"))
		})
	en.OnRegex(searchVideo).SetBlock(true).Limit(limit.LimitByGroup).Handle(handleVideo)
	en.OnRegex(searchDynamic).SetBlock(true).Limit(limit.LimitByGroup).Handle(handleDynamic)
	en.OnRegex(searchArticle).SetBlock(true).Limit(limit.LimitByGroup).Handle(handleArticle)
	en.OnRegex(searchLiveRoom).SetBlock(true).Limit(limit.LimitByGroup).Handle(handleLive)
}

func handleVideo(ctx *zero.Ctx) {
	id := ctx.State["regex_matched"].([]string)[1]
	if id == "" {
		id = ctx.State["regex_matched"].([]string)[2]
	}
	card, err := bz.GetVideoInfo(id)
	if err != nil {
		ctx.SendChain(message.Text("ERROR: ", err))
		return
	}
	msg, err := card.ToVideoMessage()
	if err != nil {
		ctx.SendChain(message.Text("ERROR: ", err))
		return
	}
	c, ok := ctx.State["manager"].(*ctrl.Control[*zero.Ctx])
	if ok && c.GetData(ctx.Event.GroupID)&enableVideoSummary == enableVideoSummary {
		summaryMsg, err := getVideoSummary(cfg, card)
		if err != nil {
			msg = append(msg, message.Text("ERROR: ", err))
		} else {
			msg = append(msg, summaryMsg...)
		}
	}
	ctx.SendChain(msg...)
	if ok && c.GetData(ctx.Event.GroupID)&enableVideoDownload == enableVideoDownload {
		downLoadMsg, err := getVideoDownload(cfg, card, cachePath)
		if err != nil {
			ctx.SendChain(message.Text("ERROR: ", err))
			return
		}
		ctx.SendChain(downLoadMsg...)
	}
}

func handleDynamic(ctx *zero.Ctx) {
	msg, err := cfg.GetDetailMessage(ctx.State["regex_matched"].([]string)[2])
	if err != nil {
		ctx.SendChain(message.Text("ERROR: ", err))
		return
	}
	ctx.SendChain(msg...)
}

func handleArticle(ctx *zero.Ctx) {
	card, err := bz.GetArticleInfo(ctx.State["regex_matched"].([]string)[1])
	if err != nil {
		ctx.SendChain(message.Text("ERROR: ", err))
		return
	}
	ctx.SendChain(card.ToArticleMessage(ctx.State["regex_matched"].([]string)[1])...)
}

func handleLive(ctx *zero.Ctx) {
	cookie, err := cfg.Load()
	if err != nil {
		ctx.SendChain(message.Text("ERROR: ", err))
		return
	}
	card, err := bz.GetLiveRoomInfo(ctx.State["regex_matched"].([]string)[1], cookie)
	if err != nil {
		ctx.SendChain(message.Text("ERROR: ", err))
		return
	}
	ctx.SendChain(card.ToMessage()...)
}

// getVideoSummary AI视频总结
func getVideoSummary(cookiecfg *bz.CookieConfig, card bz.Card) (msg []message.Segment, err error) {
	var (
		data         []byte
		videoSummary bz.VideoSummary
	)
	data, err = web.RequestDataWithHeaders(web.NewDefaultClient(), bz.SignURL(fmt.Sprintf(bz.VideoSummaryURL, card.BvID, card.CID, card.Owner.Mid)), "GET", func(req *http.Request) error {
		if cookiecfg != nil {
			cookie := ""
			cookie, err = cookiecfg.Load()
			if err != nil {
				return err
			}
			req.Header.Add("cookie", cookie)
		}
		req.Header.Set("User-Agent", ua)
		return nil
	}, nil)
	if err != nil {
		return
	}
	err = json.Unmarshal(data, &videoSummary)
	msg = make([]message.Segment, 0, 16)
	if videoSummary.Data.ModelResult.Summary == `` {
		msg = append(msg, message.Text(fmt.Sprintf("生成视频总结: %s(%d)", videoSummary.Message, videoSummary.Code)))
		return
	}
	msg = append(msg, message.Text("已为你生成视频总结\n\n"))
	msg = append(msg, message.Text(videoSummary.Data.ModelResult.Summary, "\n\n"))
	for _, v := range videoSummary.Data.ModelResult.Outline {
		msg = append(msg, message.Text("● ", v.Title, "\n"))
		for _, p := range v.PartOutline {
			msg = append(msg, message.Text(fmt.Sprintf("%d:%d %s\n", p.Timestamp/60, p.Timestamp%60, p.Content)))
		}
		msg = append(msg, message.Text("\n"))
	}
	return
}

func getVideoDownload(cookiecfg *bz.CookieConfig, card bz.Card, cachePath string) (msg []message.Segment, err error) {
	files, _ := os.ReadDir(cachePath)
        for _, f := range files {
                if info, _ := f.Info(); info != nil {
                        if time.Since(info.ModTime()) > 24*time.Hour {
                                _ = os.Remove(cachePath + f.Name())
                        }
                }
        }
	var (
		data          []byte
		videoDownload bz.VideoDownload
		stderr        bytes.Buffer
	)
	today := time.Now().Format("20060102")
	videoFile := fmt.Sprintf("%s%s%s.mp4", cachePath, card.BvID, today)
	if file.IsExist(videoFile) {
		msg = append(msg, message.Video("file:///"+file.BOTPATH+"/"+videoFile))
		return
	}
	data, err = web.RequestDataWithHeaders(web.NewDefaultClient(), bz.SignURL(fmt.Sprintf(bz.VideoDownloadURL, card.BvID, card.CID)), "GET", func(req *http.Request) error {
		if cookiecfg != nil {
			cookie := ""
			cookie, err = cookiecfg.Load()
			if err != nil {
				return err
			}
			req.Header.Add("cookie", cookie)
		}
		req.Header.Set("User-Agent", ua)
		return nil
	}, nil)
	if err != nil {
		return
	}
	err = json.Unmarshal(data, &videoDownload)
	if err != nil {
		return
	}
	headers := fmt.Sprintf("User-Agent: %s\nReferer: %s", ua, bilibiliparseReferer)

        // 下载所有分片，逐个下载后合并
        partFiles := make([]string, 0, len(videoDownload.Data.Durl))
        var concatContent strings.Builder
        totalMs := 0

        for i, d := range videoDownload.Data.Durl {
                // 限制最多下载30分钟视频
                if totalMs >= 1800000 {
                        break
                }
                partFile := fmt.Sprintf("%s%s_part%d_%s.mp4", cachePath, card.BvID, i, today)
                partFiles = append(partFiles, partFile)
                fmt.Fprintf(&concatContent, "file '%s'\n", partFile)

                duration := d.Length / 1000
                if totalMs+d.Length > 480000 {
                        duration = (480000 - totalMs) / 1000
                }
                totalMs += d.Length

                cmd := exec.Command("ffmpeg", "-headers", headers, "-i", d.URL,
                        "-t", strconv.Itoa(duration), "-c", "copy", partFile)
                cmd.Stderr = &stderr
                if err = cmd.Run(); err != nil {
                        // 清理已下载的临时文件
                        for _, f := range partFiles {
                                os.Remove(f)
                        }
                        err = errors.Errorf("下载视频分片失败，%v", stderr)
                        return
                }
        }

        // 如果只有一个分片，直接重命名即可
        if len(partFiles) == 1 {
                if err = os.Rename(partFiles[0], videoFile); err != nil {
      				for _, f := range partFiles {
          			os.Remove(f)
      				}
     			 return
  				}
        } else {
                // 多分片：写入 concat 列表并合并
                concatFile := fmt.Sprintf("%s%s_%s_concat.txt", cachePath, card.BvID, today)
                if err = os.WriteFile(concatFile, []byte(concatContent.String()), 0644); err != nil {
                        return
                }
                cmd := exec.Command("ffmpeg", "-f", "concat", "-safe", "0", "-i", concatFile,
                        "-c", "copy", videoFile)
                cmd.Stderr = &stderr
                if err = cmd.Run(); err != nil {
                        err = errors.Errorf("合并视频分片失败，%v", stderr)
                }
                // 清理
                for _, f := range partFiles {
                        os.Remove(f)
                }
                os.Remove(concatFile)
                if err != nil {
                        return
                }
        }

        msg = append(msg, message.Video("file:///"+file.BOTPATH+"/"+videoFile))
	return
}
