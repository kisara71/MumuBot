package prompt

import (
	"fmt"
	"strings"
)

func (b *Builder) writeSharedContextBlocks(out *strings.Builder, ctx *Context) {
	if ctx != nil && len(ctx.JargonMatches) > 0 {
		out.WriteString("\n术语：\n")
		for term, meaning := range ctx.JargonMatches {
			out.WriteString(fmt.Sprintf("- %s: %s\n", term, meaning))
		}
	}
	if ctx != nil && len(ctx.RelatedMemories) > 0 {
		out.WriteString("\n相关记忆：\n")
		for _, mem := range ctx.RelatedMemories {
			out.WriteString(fmt.Sprintf("- [%s] %s\n", mem.CreatedAt.Format("2006-01-02"), mem.Content))
		}
	}
	if ctx != nil && len(ctx.CrossGroupExperiences) > 0 {
		out.WriteString("\n别处相关经历：\n")
		for _, mem := range ctx.CrossGroupExperiences {
			out.WriteString(fmt.Sprintf("- [%s] %s\n", mem.CreatedAt.Format("2006-01-02"), mem.Content))
		}
	}
}
