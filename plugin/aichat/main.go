// Package aichat 大模型聊天和Agent
package aichat

import (
	"encoding/json"
	"math/rand"
	"strings"

	"github.com/RomiChan/syncx"
	"github.com/fumiama/deepinfra"
	goba "github.com/fumiama/go-onebot-agent"
	"github.com/sirupsen/logrus"

	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/extension/single"
	"github.com/wdvxdr1123/ZeroBot/message"

	"github.com/FloatTech/AnimeAPI/airecord"
	"github.com/FloatTech/floatbox/process"
	ctrl "github.com/FloatTech/zbpctrl"
	"github.com/FloatTech/zbputils/chat"
	"github.com/FloatTech/zbputils/control"
	"github.com/FloatTech/zbputils/ctxext"

	"github.com/FloatTech/ZeroBot-Plugin/plugin/aichat/focus"
	"github.com/FloatTech/ZeroBot-Plugin/plugin/aichat/memory"
)

var (
	// en data [8 temp] [8 rate] LSB
	en = control.AutoRegister(&ctrl.Options[*zero.Ctx]{
		DisableOnDefault: false,
		Extra:            control.ExtraFromString("aichat"),
		Brief:            "大模型聊天和Agent",
		Help: "- (随意聊天, 概率匹配)\n" +
			"- 默认使用专注模式: 只回应@它(或被抽中)的那句话, 以该用户与它的对话为主要上下文, 群聊背景仅作低权重参考\n" +
			"- 设置AI聊天(不)使用专注模式 (群管理, 不使用时恢复全群上下文/Agent)\n" +
			"- 专注模式下默认开启长期记忆(人物印象+日记), 见 aichatcfg 的记忆相关命令",

		PrivateDataFolder: "aichat",
	}).ApplySingle(single.New(
		single.WithKeyFn(func(ctx *zero.Ctx) int64 {
			if ctx.Event.GroupID == 0 {
				return -ctx.Event.UserID
			}
			return ctx.Event.GroupID
		}),
		// no post option, silently quit
	))
)

var (
	fastfailnorecord = false
)

func init() {
	en.OnMessage(chat.EnsureConfig, func(ctx *zero.Ctx) bool {
		stor, ok := ctx.State[zero.StateKeyPrefixKeep+"aichatcfg_stor__"].(chat.Storage)
		if !ok {
			logrus.Warnln("ERROR: cannot get stor")
			return false
		}
		mp := ctx.State[control.StateKeySyncxState].(*syncx.Map[string, any])
		if _, ok := mp.Load(chat.StateKeyAgentHooked); !ok && !stor.NoAgent() && !focus.Uses(ctxext.Storage(stor)) {
			logrus.Infoln("[aichat] skip agent for ctx has not been hooked by agent")
			return false
		}
		if !(ctx.ExtractPlainText() != "" &&
			(!stor.NoReplyAt() || (stor.NoReplyAt() && !ctx.Event.IsToMe))) {
			return false
		}
		// 专注模式下: /指令交给插件; 已有插件回复过这条消息就不再插嘴
		if focus.Uses(ctxext.Storage(stor)) && (focus.IsCommand(ctx) || focus.RepliedByOthers(ctx)) {
			return false
		}
		rate := stor.Rate()
		if !ctx.Event.IsToMe && rand.Intn(100) >= int(rate) {
			return false
		}
		if ctx.Event.IsToMe {
			ctx.Block()
		}
		return true
	}).SetBlock(false).Handle(func(ctx *zero.Ctx) {
		gid := ctx.Event.GroupID
		if gid == 0 {
			gid = -ctx.Event.UserID
		}
		stor := ctx.State[zero.StateKeyPrefixKeep+"aichatcfg_stor__"].(chat.Storage)
		temperature := stor.Temp()
		topp, maxn := chat.AC.MParams()
		mp := ctx.State[control.StateKeySyncxState].(*syncx.Map[string, any])

		if focus.Uses(ctxext.Storage(stor)) {
			focusChat(ctx, stor, gid, temperature, topp, maxn)
			return
		}

		logrus.Debugln("[aichat] agent mode test: noagent", stor.NoAgent(), "hasapi", chat.AC.AgentAPI != "", "hasmodel", chat.AC.AgentModelName != "")
		if !stor.NoAgent() && chat.AC.AgentAPI != "" && chat.AC.AgentModelName != "" && chat.AC.Key != "" {
			logrus.Debugln("[aichat] enter agent mode")
			// ================= 终极修复：同步配置并清除失效的 Agent 缓存 =================
			needReset := false
			if chat.AC.AgentChar != "" && chat.AgentCharConfig.Chars != chat.AC.AgentChar {
				chat.AgentCharConfig.Chars = chat.AC.AgentChar
				needReset = true
			}
			if chat.AC.AgentSex != "" && chat.AgentCharConfig.Sex != chat.AC.AgentSex {
				chat.AgentCharConfig.Sex = chat.AC.AgentSex
				needReset = true
			}
			if needReset {
				logrus.Debugln("[aichat] 检测到 Agent 配置不同步，更新内存并重置缓存")
				chat.ResetAgents() // 关键：清空旧的 Agent 实例，迫使其使用新提示词重新生成！
			}
			// ===============================================================================
			x := deepinfra.NewAPI(chat.AC.AgentAPI, string(chat.AC.AgentKey))
			mod, err := chat.AC.Type.Protocol(chat.AC.AgentModelName, temperature, topp, maxn, chat.AC.ReasoningEffort)
			if err != nil {
				logrus.Warnln("ERROR: ", err)
				return
			}
			role := goba.PermRoleUser
			if zero.AdminPermission(ctx) {
				role = goba.PermRoleAdmin
				if zero.SuperUserPermission(ctx) {
					role = goba.PermRoleOwner
				}
			}
			c, ok := ctx.State["manager"].(*ctrl.Control[*zero.Ctx])
			if !ok {
				logrus.Warnln("ERROR: cannot get ctrl mamager")
			}
			ag := chat.AgentOf(ctx.Event.SelfID, c.Service)
			logrus.Debugln("[aichat] got agent")
			if chat.AC.ImageAPI != "" && !ag.CanViewImage() {
				mod, err := chat.AC.ImageType.Protocol(chat.AC.ImageModelName, temperature, topp, maxn, chat.AC.ReasoningEffort)
				if err != nil {
					logrus.Warnln("ERROR: ", err)
					return
				}
				ag.SetViewImageAPI(deepinfra.NewAPI(chat.AC.ImageAPI, string(chat.AC.ImageKey)), mod)
				logrus.Debugln("[aichat] agent set img")
			}
			ctx.NoTimeout()
			logrus.Debugln("[aichat] agent set no timeout")
			hasresp := false
			for i := 0; i < 8; i++ { // 最大运行 8 轮因为问答上下文只有 16
				reqs := chat.CallAgent(ag, zero.SuperUserPermission(ctx), i+1, x, mod, gid, role)
				if len(reqs) == 0 {
					logrus.Debugln("[aichat] agent call got empty response")
					break
				}
				hasresp = true
				mp.Store(chat.StateKeyAgentTriggered, struct{}{})
				for _, req := range reqs {
					if req.Action == goba.SVM { // is a fake action
						continue
					}
					logrus.Debugln("[chat] agent triggered", gid, "add requ:", &req)
					ag.AddRequest(gid, &req)
					rsp := ctx.CallAction(req.Action, req.Params)
					logrus.Debugln("[chat] agent triggered", gid, "add resp:", &rsp)
					ag.AddResponse(gid, &goba.APIResponse{
						Status:  rsp.Status,
						Data:    json.RawMessage(rsp.Data.Raw),
						Message: rsp.Message,
						Wording: rsp.Wording,
						RetCode: rsp.RetCode,
					})
				}
			}
			if hasresp {
				return
			}
			// no response, fall back to normal chat
			logrus.Debugln("[aichat] agent fell back to normal chat")
		}

		x := deepinfra.NewAPI(chat.AC.API, string(chat.AC.Key))
		mod, err := chat.AC.Type.Protocol(chat.AC.ModelName, temperature, topp, maxn, chat.AC.ReasoningEffort)
		if err != nil {
			logrus.Warnln("ERROR: ", err)
			return
		}
		data, err := x.Request(chat.GetChatContext(mod, gid, chat.AC.SystemP, bool(chat.AC.NoSystemP)))
		if err != nil {
			logrus.Warnln("[aichat] post err:", err)
			return
		}

		txt := chat.Sanitize(strings.Trim(data, "\n 　"))
		if len(txt) > 0 {
			chat.AddChatReply(gid, txt)
			sendReply(ctx, stor, txt)
		}
	})
}

// focusChat 专注模式: 只回应当前这句话, 群聊背景仅作低权重参考
func focusChat(ctx *zero.Ctx, stor chat.Storage, gid int64, temperature, topp float32, maxn uint) {
	char := chat.AC.AgentChar
	if char == "" {
		char = chat.AgentCharConfig.Chars
	}
	sex := chat.AC.AgentSex
	if sex == "" {
		sex = chat.AgentCharConfig.Sex
	}
	persona := focus.Persona(zero.BotConfig.NickName, sex, char, zero.BotConfig.SuperUsers, ctx.Event.IsToMe)
	req := focus.NewRequest(ctx, persona, bool(chat.AC.NoSystemP))
	if req == nil {
		return
	}
	if ctx.Event.GroupID != 0 && ctx.Event.IsToMe {
		req.AddGroupRules(zero.AdminPermission(ctx))
	}
	usemem := memory.Enabled(ctxext.Storage(stor))
	if usemem {
		rc := memory.Get(gid, ctx.Event.UserID, req.Text())
		req.SetMemory(rc.Profile, rc.Items)
	}
	x := deepinfra.NewAPI(chat.AC.API, string(chat.AC.Key))
	// 开启推理时推理 token 也计入 max_tokens, 此时不收紧以免正文被挤掉
	if strings.EqualFold(chat.AC.ReasoningEffort, "none") && maxn > focus.MaxTokens {
		maxn = focus.MaxTokens
	}
	txt := ""
	var acts []focus.Action
	// 回复自曝 AI 身份时重试一次, 仍然自曝就不发
	for try := 0; try < 2; try++ {
		mod, err := chat.AC.Type.Protocol(chat.AC.ModelName, temperature, topp, maxn, chat.AC.ReasoningEffort)
		if err != nil {
			logrus.Warnln("ERROR: ", err)
			return
		}
		data, err := x.Request(req.Modelize(mod))
		if err != nil {
			logrus.Warnln("[aichat] focus post err:", err)
			return
		}
		if focus.IsPass(data) {
			logrus.Debugln("[aichat] focus model chose to pass in", gid)
			return
		}
		// 先取出动作标记再清洗, Sanitize 会按 "]" "】" 截断文本
		clean, a := req.ParseActions(data)
		txt = focus.Clamp(chat.Sanitize(strings.Trim(clean, "\n 　")))
		if !focus.LeaksAI(txt) {
			acts = a
			break
		}
		logrus.Infoln("[aichat] focus reply leaks AI identity, try", try+1, ":", txt)
		txt = ""
	}
	if focus.RepliedByOthers(ctx) || (len(txt) == 0 && len(acts) == 0) {
		return
	}
	if len(txt) > 0 {
		req.Done(txt)
		if usemem {
			memory.Observe(gid, ctx.Event.UserID, ctx.Event.Sender.Name(), req.Text(), txt)
		}
		chat.AddChatReply(gid, txt)
		sendReply(ctx, stor, txt)
	}
	runActions(ctx, req, acts)
}

func isSuperUser(qq int64) bool {
	for _, su := range zero.BotConfig.SuperUsers {
		if su == qq {
			return true
		}
	}
	return false
}

// runActions 执行模型给出的群管理动作. 与旧 Agent 的权限一致: 只有群管理/群主/主人@bot 时才能让它禁言别人,
// 且目标只能是当前消息 @ 到的人或被引用消息的发送者, 主人、bot 自己和群管理不能被禁言.
func runActions(ctx *zero.Ctx, req *focus.Request, acts []focus.Action) {
	if len(acts) == 0 {
		return
	}
	gid := ctx.Event.GroupID
	if gid == 0 || !ctx.Event.IsToMe || !zero.AdminPermission(ctx) {
		logrus.Infoln("[aichat] focus drop actions from non-admin", ctx.Event.UserID, "in", gid, ":", acts)
		return
	}
	for _, a := range acts {
		if !req.CanTarget(a.QQ) || a.QQ == ctx.Event.SelfID || isSuperUser(a.QQ) {
			logrus.Infoln("[aichat] focus refuse action on", a.QQ, "in", gid, "requested by", ctx.Event.UserID)
			continue
		}
		var secs int64
		if a.Ban {
			if role := ctx.GetGroupMemberInfo(gid, a.QQ, true).Get("role").String(); role != "member" {
				logrus.Infoln("[aichat] focus refuse to ban", a.QQ, "role", role, "in", gid)
				continue
			}
			secs = a.Minutes * 60
		}
		rsp := ctx.CallAction("set_group_ban", zero.Params{"group_id": gid, "user_id": a.QQ, "duration": secs})
		if rsp.Status != "ok" {
			logrus.Warnln("[aichat] focus set_group_ban", a.QQ, secs, "failed:", rsp.RetCode, rsp.Message, rsp.Wording)
			ctx.SendChain(message.Text("诶…没弄成，可能我在这个群没有管理权限"))
			continue
		}
		logrus.Infoln("[aichat] focus set_group_ban", a.QQ, "for", secs, "s in", gid, "requested by", ctx.Event.UserID)
	}
}

// sendReply 处理 {name}/{me}/{segment} 占位并发送, 可选以 AI 语音输出
func sendReply(ctx *zero.Ctx, stor chat.Storage, txt string) {
	nick := zero.BotConfig.NickName[rand.Intn(len(zero.BotConfig.NickName))]
	txt = strings.ReplaceAll(txt, "{name}", ctx.CardOrNickName(ctx.Event.UserID))
	txt = strings.ReplaceAll(txt, "{me}", nick)
	id := any(nil)
	if ctx.Event.IsToMe {
		id = ctx.Event.MessageID
	}
	for _, t := range strings.Split(txt, "{segment}") {
		if t == "" {
			continue
		}
		logrus.Debugln("[aichat] 回复内容:", t)
		recCfg := airecord.GetConfig()
		record := ""
		if !fastfailnorecord && !stor.NoRecord() {
			record = ctx.GetAIRecord(recCfg.ModelID, recCfg.Customgid, t)
			if record != "" {
				ctx.SendChain(message.Record(record))
				continue
			}
			fastfailnorecord = true
		}
		if id != nil {
			id = ctx.SendChain(message.Reply(id), message.Text(t))
		} else {
			id = ctx.SendChain(message.Text(t))
		}
		process.SleepAbout1sTo2s()
	}
}
