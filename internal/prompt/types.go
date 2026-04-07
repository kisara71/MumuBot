package prompt

import (
	"mumu-bot/internal/memory"
	"mumu-bot/internal/session"
)

type MoodInfo struct {
	Valence     float64
	Energy      float64
	Sociability float64
	Irritation  float64
	Curiosity   float64
}

type Context struct {
	Ref                   session.Ref
	MoodState             *MoodInfo
	JargonMatches         map[string]string
	GroupInfo             string
	PeerInfo              string
	LoopInfo              string
	RelatedMemories       []memory.Memory
	CrossGroupExperiences []memory.Memory
	StyleHints            []string
}

type BuildInput struct {
	Context      *Context
	ChatContext  string
	ExtraPrompt  string
	RecentPeople string
	IsMention    bool
}
