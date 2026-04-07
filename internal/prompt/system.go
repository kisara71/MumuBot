package prompt

import (
	"fmt"
	"strings"
)

func (b *Builder) groupSystemPrompt() string {
	var out strings.Builder
	interests := strings.Join(b.cfg.Interests, "、")

	out.WriteString(fmt.Sprintf("你是%s，QQ群里的普通群友，QQ号%d。你通过工具与QQ客户端交互，不是助手、客服或机器人。\n", b.cfg.Name, b.cfg.QQ))
	out.WriteString(fmt.Sprintf("名字：%s。更容易接话的话题：%s。\n", b.cfg.Name, interests))
	out.WriteString("说话风格：" + b.cfg.SpeakingStyle + "\n")
	if b.cfg.Personality != "" {
		out.WriteString("补充设定：" + b.cfg.Personality + "\n")
	}
	out.WriteString(groupSystemRules)
	return out.String()
}

func (b *Builder) privateSystemPrompt() string {
	var out strings.Builder
	interests := strings.Join(b.cfg.Interests, "、")

	out.WriteString(fmt.Sprintf("你是%s，正在和单个用户一对一私聊，QQ号%d。你通过工具与QQ客户端交互，不是助手、客服或机器人。\n", b.cfg.Name, b.cfg.QQ))
	out.WriteString(fmt.Sprintf("名字：%s。更容易接话的话题：%s。\n", b.cfg.Name, interests))
	out.WriteString("说话风格：" + b.cfg.SpeakingStyle + "\n")
	if b.cfg.Personality != "" {
		out.WriteString("补充设定：" + b.cfg.Personality + "\n")
	}
	out.WriteString(privateSystemRules)
	return out.String()
}
