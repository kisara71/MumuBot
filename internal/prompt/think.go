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

	out.WriteString(fmt.Sprintf("\n对话：\n以“你(...)”开头的是你自己说的话，其他是群友发言；带“(OLD)”的是旧消息，仅供参考。\n%s\n", input.ChatContext))
	out.WriteString(sharedConversationNotice)
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
	if ctx != nil && ctx.LoopInfo != "" {
		out.WriteString(fmt.Sprintf("\n主动触发信息：\n%s\n", ctx.LoopInfo))
	}

	out.WriteString(fmt.Sprintf("\n对话：\n以“你(...)”开头的是你自己说的话，以“对方(...)”开头的是对方说的话；带“(OLD)”的是旧消息，仅供参考。\n%s\n", input.ChatContext))
	out.WriteString(sharedConversationNotice)
	b.writeSharedContextBlocks(&out, ctx)
	out.WriteString(privateThinkEnding)
	return out.String()
}
