package prompt

import (
	"github.com/kisara71/luma/internal/memory"
)

type MoodInfo struct {
	Valence, Energy, Sociability, Irritation, Curiosity float64
}

type Context struct {
	MoodState       *MoodInfo
	PeerInfo        string
	ProactiveInfo   string
	RelatedMemories []memory.Memory
}

type BuildInput struct {
	Context     *Context
	IsFirst     bool
	ChatContext string
	ExtraPrompt string
}
