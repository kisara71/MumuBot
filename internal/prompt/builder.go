package prompt

import (
	"mumu-bot/internal/config"
	"mumu-bot/internal/session"
)

type Builder struct {
	cfg *config.PersonaConfig
}

func NewBuilder(cfg *config.PersonaConfig) *Builder {
	return &Builder{cfg: cfg}
}

func (b *Builder) BuildSystemPrompt(ref session.Ref) string {
	if ref.Source == session.MessageSourcePrivate {
		return b.privateSystemPrompt()
	}
	return b.groupSystemPrompt()
}

func (b *Builder) BuildThinkPrompt(input BuildInput) string {
	ctx := input.Context
	if ctx != nil && ctx.Ref.Source == session.MessageSourcePrivate {
		return b.privateThinkPrompt(input)
	}
	return b.groupThinkPrompt(input)
}
