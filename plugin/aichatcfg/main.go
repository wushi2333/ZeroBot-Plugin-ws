// Package aichatcfg aichat 的配置, 优先级要比 aichat 高
package aichatcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
	zero "github.com/wdvxdr1123/ZeroBot"
	"github.com/wdvxdr1123/ZeroBot/message"

	ctrl "github.com/FloatTech/zbpctrl"
	"github.com/FloatTech/zbputils/chat"
	"github.com/FloatTech/zbputils/control"
	"github.com/FloatTech/zbputils/ctxext"

	"github.com/FloatTech/ZeroBot-Plugin/plugin/aichat/focus"
	"github.com/FloatTech/ZeroBot-Plugin/plugin/aichat/memory"
	"github.com/FloatTech/ZeroBot-Plugin/plugin/aichat/shortctx"
	"github.com/FloatTech/ZeroBot-Plugin/plugin/aichat/websearch"
)

var (
	// en data [8 temp] [8 rate] LSB
	en = control.AutoRegister(&ctrl.Options[*zero.Ctx]{
		DisableOnDefault: false,
		Extra:            control.ExtraFromString("aichat"),
		Brief:            "aichat 的配置",
		Help: "- 设置AI聊天触发概率10\n" +
			"- 设置AI聊天温度80\n" +
			"- 设置AI聊天(|识图|Agent)接口类型[OpenAI|OLLaMA|GenAI]\n" +
			"- 设置AI聊天(不)使用Agent模式\n" +
			"- 设置AI聊天(不)使用专注模式 (默认使用, 只回应触发的那句话)\n" +
			"- 设置AI聊天(不)使用记忆 (默认使用, 仅专注模式下生效)\n" +
			"- 设置AI聊天(不)使用群聊上下文 (超级用户, 默认关闭: 最近约200条群聊原文+12小时摘要)\n" +
			"- 设置AI聊天思考强度[no|low|high] (超级用户, 按群设置, 默认no不思考)\n" +
			"- 设置AI聊天(不)使用联网搜索 (超级用户, 默认使用: 被@时遇到不知道的事会先上网查)\n" +
			"- 查看我的AI记忆 | @bot 你记得我什么\n" +
			"- 忘掉我的AI记忆\n" +
			"- 查看本群AI记忆 | 清空本群AI记忆 (群管理)\n" +
			"- 设置AI聊天(不)支持系统提示词\n" +
			"- 设置AI聊天(|识图|Agent)接口地址https://api.siliconflow.cn/v1/chat/completions\n" +
			"- 设置AI聊天(|识图|Agent)密钥xxx\n" +
			"- 设置AI聊天(|识图|Agent)模型名Qwen/Qwen3-8B\n" +
			"- 查看AI聊天系统提示词\n" +
			"- 重置AI聊天系统提示词\n" +
			"- 设置AI聊天系统提示词xxx\n" +
			"- 设置AI聊天Agent性格xxx" +
			"- 查看AI聊天Agent性格" +
			"- 设置AI聊天Agent性别xxx" +
			"- 查看AI聊天Agent性别" +
			"- 重置AI聊天Agent性格性别\n" +
			"- 设置AI聊天分隔符</think>(留空则清除)\n" +
			"- 设置AI聊天(不)响应AT\n" +
			"- 设置AI聊天最大长度4096\n" +
			"- 设置AI聊天TopP 0.9\n" +
			"- 设置AI聊天(不)以AI语音输出\n" +
			"- 设置AI聊天努力度none(留空则清除, 全局, AI聊天已改用按群的思考强度)\n" +
			"- 查看AI聊天配置\n" +
			"- 重置AI聊天Agent\n" +
			"- 重置AI聊天\n",
	})
)

var agentCfgFile = filepath.Join("data", "aichat", "agent_char.json") // 专属配置文件路径

func loadAgentCfg() {
	b, err := os.ReadFile(agentCfgFile)
	if err == nil {
		var data struct {
			Chars string `json:"chars"`
			Sex   string `json:"sex"`
		}
		if json.Unmarshal(b, &data) == nil {
			// 将读出的数据强制赋给底层 Agent 配置和全局 AC 变量
			if data.Chars != "" {
				chat.AgentCharConfig.Chars = data.Chars
				chat.AC.AgentChar = data.Chars
			}
			if data.Sex != "" {
				chat.AgentCharConfig.Sex = data.Sex
				chat.AC.AgentSex = data.Sex
			}
		}
	}
}

// effortName 思考强度的显示名
func effortName(e string) string {
	if e == "none" {
		return "no (不思考)"
	}
	return e
}

// groupOf 私聊时返回 -uid
func groupOf(ctx *zero.Ctx) int64 {
	if ctx.Event.GroupID == 0 {
		return -ctx.Event.UserID
	}
	return ctx.Event.GroupID
}

func saveAgentCfg() {
	_ = os.MkdirAll(filepath.Dir(agentCfgFile), 0755)
	data := struct {
		Chars string `json:"chars"`
		Sex   string `json:"sex"`
	}{
		Chars: chat.AgentCharConfig.Chars,
		Sex:   chat.AgentCharConfig.Sex,
	}
	b, _ := json.Marshal(data)
	_ = os.WriteFile(agentCfgFile, b, 0644)
}

func init() {
	loadAgentCfg()

	en.UsePreHandler(chat.EnsureConfig, func(ctx *zero.Ctx) bool {
		k := zero.StateKeyPrefixKeep + "aichatcfg_stor__"
		if _, ok := ctx.State[k]; ok {
			return true
		}
		gid := ctx.Event.GroupID
		if gid == 0 {
			gid = -ctx.Event.UserID
		}
		stor, err := chat.NewStorage(ctx, gid)
		if err != nil {
			logrus.Warnln("ERROR: ", err)
			return false
		}
		ctx.State[k] = stor
		return true
	})
	en.OnPrefix("设置AI聊天触发概率", zero.AdminPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBitmapHandler(chat.BitmapRate, 0, 100))
	en.OnPrefix("设置AI聊天温度", zero.AdminPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBitmapHandler(chat.BitmapTemp, 0, 100))
	en.OnPrefix("设置AI聊天接口类型", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetModelType(&chat.AC.Type))
	en.OnPrefix("设置AI聊天识图接口类型", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetModelType(&chat.AC.ImageType))
	en.OnPrefix("设置AI聊天Agent接口类型", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetModelType(&chat.AC.AgentType))
	en.OnPrefix("设置AI聊天接口地址", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.API))
	en.OnPrefix("设置AI聊天识图接口地址", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.ImageAPI))
	en.OnPrefix("设置AI聊天Agent接口地址", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.AgentAPI))
	en.OnPrefix("设置AI聊天密钥", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.Key))
	en.OnPrefix("设置AI聊天识图密钥", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.ImageKey))
	en.OnPrefix("设置AI聊天Agent密钥", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.AgentKey))
	en.OnPrefix("设置AI聊天模型名", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.ModelName))
	en.OnPrefix("设置AI聊天识图模型名", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.ImageModelName))
	en.OnPrefix("设置AI聊天Agent模型名", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.AgentModelName))
	en.OnPrefix("设置AI聊天系统提示词", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.SystemP))
	en.OnPrefix("设置AI聊天Agent性格", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.AgentChar), func(_ *zero.Ctx) {
			chat.AgentCharConfig.Chars = chat.AC.AgentChar
			saveAgentCfg() // 【新增】：保存到本地专属文件
			chat.ResetAgents()
		})
	en.OnPrefix("设置AI聊天Agent性别", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.AgentSex), func(_ *zero.Ctx) {
			chat.AgentCharConfig.Sex = chat.AC.AgentSex
			saveAgentCfg()     // 【新增】：保存到本地专属文件
			chat.ResetAgents() // 【新增】：清空旧 Agent 缓存
		})
	en.OnFullMatch("查看AI聊天系统提示词", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		ctx.SendChain(message.Text(chat.AC.SystemP))
	})
	en.OnFullMatch("查看AI聊天Agent性格", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		ctx.SendChain(message.Text(chat.AC.AgentChar))
	})
	en.OnFullMatch("重置AI聊天系统提示词", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		c, ok := ctx.State["manager"].(*ctrl.Control[*zero.Ctx])
		if !ok {
			ctx.SendChain(message.Text("ERROR: no such plugin"))
			return
		}
		chat.AC.SystemP = chat.SystemPrompt
		err := c.SetExtra(&chat.AC)
		if err != nil {
			ctx.SendChain(message.Text("ERROR: set extra err: ", err))
			return
		}
		ctx.SendChain(message.Text("成功"))
	})
	en.OnFullMatch("重置AI聊天Agent性格性别", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		c, ok := ctx.State["manager"].(*ctrl.Control[*zero.Ctx])
		if !ok {
			ctx.SendChain(message.Text("ERROR: no such plugin"))
			return
		}
		chat.ResetAgentCharConfig()

		chat.AC.AgentChar = chat.AgentCharConfig.Chars
		chat.AC.AgentSex = chat.AgentCharConfig.Sex
		saveAgentCfg()
		chat.ResetAgents()

		err := c.SetExtra(&chat.AC)
		if err != nil {
			ctx.SendChain(message.Text("ERROR: set extra err: ", err))
			return
		}
		ctx.SendChain(message.Text("成功, 请重置AI聊天Agent"))
	})
	en.OnPrefix("设置AI聊天分隔符", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.Separator))
	en.OnRegex("^设置AI聊天(不)?响应AT$", zero.SuperUserPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBoolHandler(chat.BitmapNrat))
	en.OnRegex("^设置AI聊天(不)?支持系统提示词$", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetBool(&chat.AC.NoSystemP))
	en.OnRegex("^设置AI聊天(不)?使用Agent模式$", zero.SuperUserPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBoolHandler(chat.BitmapNagt))
	en.OnRegex("^设置AI聊天(不)?使用专注模式$", zero.AdminPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBoolHandler(focus.BitmapNfcs))
	en.OnRegex("^设置AI聊天(不)?使用记忆$", zero.AdminPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBoolHandler(memory.BitmapNmem))
	// 群聊上下文默认关闭, 只有超级用户能在群里开启 (位为 1 表示开启, 与其他"不使用"位相反)
	en.OnRegex("^设置AI聊天(不)?使用群聊上下文$", zero.OnlyGroup, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			on := ctx.State["regex_matched"].([]string)[1] != "不"
			gid := ctx.Event.GroupID
			stor, err := ctxext.NewStorage(ctx, gid)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: ", err))
				return
			}
			v := int64(0)
			if on {
				v = 1
			}
			if err := stor.Set(v, shortctx.BitmapGctx).SaveTo(ctx, gid); err != nil {
				ctx.SendChain(message.Text("ERROR: set data err: ", err))
				return
			}
			if on {
				ctx.SendChain(message.Text("本群已开启群聊上下文"))
				return
			}
			ctx.SendChain(message.Text("本群已关闭群聊上下文"))
		})
	en.OnRegex("^设置AI聊天(不)?使用联网搜索$", zero.SuperUserPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBoolHandler(websearch.BitmapNsrch))
	// 思考强度按群设置, 只有超级用户能改; 默认 no (不思考)
	en.OnRegex(`^设置AI聊天思考强度\s*(?i:(no|none|low|high))$`, zero.OnlyGroup, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			lv := strings.ToLower(ctx.State["regex_matched"].([]string)[1])
			v := int64(0)
			for i, e := range focus.Efforts {
				if e == lv {
					v = int64(i)
				}
			}
			gid := ctx.Event.GroupID
			stor, err := ctxext.NewStorage(ctx, gid)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: ", err))
				return
			}
			if err := stor.Set(v, focus.BitmapEffort).SaveTo(ctx, gid); err != nil {
				ctx.SendChain(message.Text("ERROR: set data err: ", err))
				return
			}
			ctx.SendChain(message.Text("本群AI聊天思考强度已设为 ", effortName(focus.Efforts[v])))
		})
	showmine := func(ctx *zero.Ctx) {
		profile, items := memory.About(groupOf(ctx), ctx.Event.UserID, 10)
		if profile == "" && len(items) == 0 {
			ctx.SendChain(message.Reply(ctx.Event.MessageID), message.Text("还没有关于你的记忆哦"))
			return
		}
		sb := strings.Builder{}
		if profile != "" {
			sb.WriteString("【印象】")
			sb.WriteString(profile)
		}
		for _, it := range items {
			sb.WriteString("\n- ")
			sb.WriteString(it)
		}
		ctx.SendChain(message.Reply(ctx.Event.MessageID), message.Text(strings.TrimSpace(sb.String())))
	}
	en.OnFullMatch("查看我的AI记忆").SetBlock(true).Handle(showmine)
	en.OnFullMatch("你记得我什么", zero.OnlyToMe).SetBlock(true).Handle(showmine)
	en.OnFullMatch("忘掉我的AI记忆").SetBlock(true).Handle(func(ctx *zero.Ctx) {
		if err := memory.Forget(groupOf(ctx), ctx.Event.UserID); err != nil {
			ctx.SendChain(message.Text("ERROR: ", err))
			return
		}
		ctx.SendChain(message.Reply(ctx.Event.MessageID), message.Text("已经忘掉关于你的记忆了"))
	})
	en.OnFullMatch("查看本群AI记忆", zero.OnlyGroup, zero.AdminPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		items, total, profiles := memory.Group(ctx.Event.GroupID, 20)
		sb := strings.Builder{}
		sb.WriteString("本群共有 ")
		sb.WriteString(strconv.Itoa(total))
		sb.WriteString(" 条日记、")
		sb.WriteString(strconv.Itoa(profiles))
		sb.WriteString(" 份人物印象")
		if len(items) > 0 {
			sb.WriteString("，最近的：")
		}
		for _, it := range items {
			sb.WriteString("\n- ")
			sb.WriteString(it)
		}
		ctx.SendChain(message.Text(sb.String()))
	})
	en.OnFullMatch("清空本群AI记忆", zero.OnlyGroup, zero.AdminPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		if err := memory.Forget(ctx.Event.GroupID, 0); err != nil {
			ctx.SendChain(message.Text("ERROR: ", err))
			return
		}
		ctx.SendChain(message.Text("已清空本群的AI记忆"))
	})
	en.OnPrefix("设置AI聊天最大长度", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetUint(&chat.AC.MaxN))
	en.OnPrefix("设置AI聊天TopP", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetFloat32(&chat.AC.TopP))
	en.OnRegex("^设置AI聊天(不)?以AI语音输出$", zero.AdminPermission).SetBlock(true).
		Handle(ctxext.NewStorageSaveBoolHandler(chat.BitmapNrec))
	en.OnPrefix("设置AI聊天努力度", chat.EnsureConfig, zero.OnlyPrivate, zero.SuperUserPermission).SetBlock(true).
		Handle(chat.NewExtraSetStr(&chat.AC.ReasoningEffort))
	en.OnFullMatch("查看AI聊天配置", chat.EnsureConfig, zero.SuperUserPermission).SetBlock(true).
		Handle(func(ctx *zero.Ctx) {
			gid := ctx.Event.GroupID
			if gid == 0 {
				gid = -ctx.Event.UserID
			}
			stor, err := chat.NewStorage(ctx, gid)
			if err != nil {
				ctx.SendChain(message.Text("ERROR: ", err))
				return
			}
			ctx.SendChain(
				message.Text(
					"【当前AI聊天本群配置】\n",
					"• 触发概率：", int(stor.Rate()), "\n",
					"• 温度：", stor.Temp(), "\n",
					"• 以AI语音输出：", chat.ModelBool(!stor.NoRecord()), "\n",
					"• 专注模式：", chat.ModelBool(focus.Uses(ctxext.Storage(stor))), "\n",
					"• 长期记忆(专注模式下)：", chat.ModelBool(memory.Enabled(ctxext.Storage(stor))), "\n",
					"• 群聊上下文(专注模式下)：", chat.ModelBool(shortctx.Enabled(ctxext.Storage(stor))), "\n",
					"• 思考强度：", effortName(focus.Effort(ctxext.Storage(stor))), "\n",
					"• 联网搜索(专注模式下被@时)：", chat.ModelBool(websearch.Enabled(ctxext.Storage(stor))), "\n",
					"• 使用Agent(专注模式关闭时)：", chat.ModelBool(!stor.NoAgent()), "\n",
					"• 响应@：", chat.ModelBool(!stor.NoReplyAt()), "\n",
				),
				message.Text("【当前AI聊天全局配置】\n", &chat.AC),
			)
		})
	en.OnFullMatch("重置AI聊天Agent", chat.EnsureConfig, zero.SuperUserPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		chat.ResetAgents()
		ctx.SendChain(message.Text("成功"))
	})
	en.OnFullMatch("重置AI聊天", chat.EnsureConfig, zero.SuperUserPermission).SetBlock(true).Handle(func(ctx *zero.Ctx) {
		chat.ResetChat()
		focus.Reset()
		ctx.SendChain(message.Text("成功"))
	})
}
