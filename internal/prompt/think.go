package prompt

import (
	"fmt"
	"strings"
)

func (b *Builder) groupThinkPrompt(input BuildInput) string {
	var out strings.Builder
	ctx := input.Context

	b.writeSharedThinkPrefix(&out, ctx)
	if ctx != nil && ctx.GroupInfo != "" {
		out.WriteString(fmt.Sprintf("\n群信息：\n%s\n", ctx.GroupInfo))
	}
	if input.ExtraPrompt != "" {
		out.WriteString(fmt.Sprintf("\n补充说明：\n%s\n", input.ExtraPrompt))
	}

	out.WriteString(fmt.Sprintf("\n对话：\n“你(...)”是你自己；“(OLD)”表示旧消息。\n%s\n", input.ChatContext))
	out.WriteString(sharedConversationNotice)
	out.WriteString(sharedToolOutputRule)
	b.writeSharedContextBlocks(&out, ctx)

	if ctx != nil && len(ctx.StyleHints) > 0 {
		out.WriteString("\n可参考的群聊表达习惯：\n")
		for _, hint := range ctx.StyleHints {
			out.WriteString(fmt.Sprintf("- %s\n", hint))
		}
	}
	if input.RecentPeople != "" {
		out.WriteString(fmt.Sprintf("\n最近在场的人：\n%s\n", input.RecentPeople))
	}
	if input.IsMention && ctx != nil && ctx.Ref.IsGroup() {
		out.WriteString("\n注意：有人提到你了，可能在找你说话，你可以看情况回复。\n")
	}

	out.WriteString(groupThinkEnding)
	return out.String()
}

func (b *Builder) privateThinkPrompt(input BuildInput) string {
	var out strings.Builder
	ctx := input.Context

	b.writeSharedThinkPrefix(&out, ctx)
	if ctx != nil && ctx.PeerInfo != "" {
		out.WriteString(fmt.Sprintf("\n对方信息：\n%s\n", ctx.PeerInfo))
	}
	if input.ExtraPrompt != "" {
		out.WriteString(fmt.Sprintf("\n补充说明：\n%s\n", input.ExtraPrompt))
	}
	if input.IsFirstPrivate {
		out.WriteString("\n首次私聊提示：\n")
		out.WriteString(firstPrivatePrompt)
		out.WriteString("\n")
	}
	if ctx != nil && ctx.LoopInfo != "" {
		out.WriteString(fmt.Sprintf("\n主动触发信息：\n%s\n", ctx.LoopInfo))
	}

	out.WriteString(fmt.Sprintf("\n对话：\n“你(...)”是你自己；“对方(...)”是对方；“(OLD)”表示旧消息。\n%s\n", input.ChatContext))
	out.WriteString(sharedConversationNotice)
	out.WriteString(sharedToolOutputRule)
	b.writeSharedContextBlocks(&out, ctx)
	out.WriteString(privateThinkEnding)
	return out.String()
}
