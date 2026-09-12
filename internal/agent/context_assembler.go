package agent

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/memory"
	"github.com/kisara71/luma/internal/onebot"
	"github.com/kisara71/luma/internal/prompt"
	"github.com/kisara71/luma/internal/session"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

const (
	maxPromptMessages = 24
	maxPromptRunes    = 12000
	memoryQueryTurns  = 8
)

var messageLinePattern = regexp.MustCompile(`^\[(\d{2}:\d{2}:\d{2})\] #(-?\d+) (.*?):(.*)\n?$`)

type AssembledContext struct {
	PromptContext *prompt.Context
	IsFirst       bool
	ChatContext   string
	ExtraPrompt   string
}

type ContextAssembler struct {
	memory *memory.Manager
	bot    *onebot.Client
}

func NewContextAssembler(memoryMgr *memory.Manager, bot *onebot.Client) *ContextAssembler {
	return &ContextAssembler{memory: memoryMgr, bot: bot}
}

func (a *ContextAssembler) Assemble(ctx context.Context, sess *session.Session[*onebot.Message], proactive bool) *AssembledContext {
	if sess == nil || !sess.Ref.Valid() {
		return nil
	}
	chatContext := a.assembleChatContext(sess)
	if chatContext == "" {
		return nil
	}

	result := &AssembledContext{
		PromptContext: &prompt.Context{},
		ChatContext:   chatContext,
		ExtraPrompt:   resolveExtraPrompt(sess.Ref),
	}
	if proactive {
		result.PromptContext.ProactiveInfo = a.buildProactiveContext(sess)
	}
	if config.Get().Agent.EnableActiveRetrieval {
		result.PromptContext.RelatedMemories = a.buildMemoryContext(ctx, sess)
	}
	if mood, err := a.memory.GetMoodState(sess.Ref.UserID); err == nil {
		result.PromptContext.MoodState = &prompt.MoodInfo{
			Valence: mood.Valence, Energy: mood.Energy, Sociability: mood.Sociability,
			Irritation: mood.Irritation, Curiosity: mood.Curiosity,
		}
	}
	if first, err := a.memory.IsFirstConversation(sess.Ref); err == nil {
		result.IsFirst = first
	}
	result.PromptContext.PeerInfo = a.buildPeerContext(sess)
	return result
}

func resolveExtraPrompt(ref session.Ref) string {
	if user := config.Get().GetUserConfig(ref.UserID); user != nil {
		return user.ExtraPrompt
	}
	return ""
}

func (a *ContextAssembler) buildProactiveContext(sess *session.Session[*onebot.Message]) string {
	msgs := sess.Messages()
	if len(msgs) == 0 {
		return "这次思考不是由对方的新消息触发的。"
	}
	last := msgs[len(msgs)-1]
	return fmt.Sprintf("这次思考不是由对方的新消息触发的。距离上一条消息约%s。结合你们真实的关系和最近对话，自主决定现在会做什么。", formatElapsed(time.Since(last.Time)))
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return "不到1分钟"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分钟", int(d/time.Minute))
	}
	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	if minutes == 0 {
		return fmt.Sprintf("%d小时", hours)
	}
	return fmt.Sprintf("%d小时%d分钟", hours, minutes)
}

func (a *ContextAssembler) buildPeerContext(sess *session.Session[*onebot.Message]) string {
	nickname := ""
	for i := len(sess.Messages()) - 1; i >= 0; i-- {
		msg := sess.Messages()[i]
		if msg.UserID == sess.Ref.UserID && msg.Nickname != "" {
			nickname = msg.Nickname
			break
		}
	}
	profile, err := a.memory.GetUserProfile(sess.Ref.UserID)
	if err != nil {
		return "- 对方: " + nickname
	}
	displayName := profile.PreferredName(nickname)
	parts := []string{
		"- 对方: " + displayName,
		fmt.Sprintf("- 关系: 亲密 %.2f，信任 %.2f，熟悉 %.2f", profile.Intimacy, profile.Trust, profile.Familiarity),
	}
	if profile.SpeakStyle != "" {
		parts = append(parts, "- 对方的表达习惯: "+profile.SpeakStyle)
	}
	if profile.Interests != "" {
		var interests []string
		if sonic.UnmarshalString(profile.Interests, &interests) == nil {
			parts = append(parts, "- 对方的兴趣: "+strings.Join(interests, "、"))
		}
	}
	return strings.Join(parts, "\n")
}

func (a *ContextAssembler) buildMemoryContext(ctx context.Context, sess *session.Session[*onebot.Message]) []memory.Memory {
	msgs := sortedMessages(sess.Messages())
	if len(msgs) > memoryQueryTurns {
		msgs = msgs[len(msgs)-memoryQueryTurns:]
	}
	parts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		if text := strings.TrimSpace(msg.Content); text != "" {
			parts = append(parts, text)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	memories, err := a.memory.SearchSimilarMemoriesByConversation(ctx, strings.Join(parts, "\n"), sess.Ref, "", 4, 0.72)
	if err != nil {
		zap.L().Debug("当前用户记忆检索失败", zap.Int64("user_id", sess.Ref.UserID), zap.Error(err))
		return nil
	}
	return memories
}

func (a *ContextAssembler) assembleChatContext(sess *session.Session[*onebot.Message]) string {
	msgs := sortedMessages(sess.Messages())
	if len(msgs) > maxPromptMessages {
		msgs = msgs[len(msgs)-maxPromptMessages:]
	}

	peerName := ""
	if profile, err := a.memory.GetUserProfile(sess.Ref.UserID); err == nil {
		peerName = profile.PreferredName("")
	}
	lines := make([]string, 0, len(msgs))
	used := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		line := renderPromptMessageLine(msg, msg.FinalContent, peerName)
		runes := []rune(line)
		remaining := maxPromptRunes - used
		if remaining <= 0 {
			break
		}
		if len(runes) > remaining {
			runes = runes[len(runes)-remaining:]
		}
		lines = append(lines, string(runes))
		used += len(runes)
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return strings.Join(lines, "")
}

func sortedMessages(msgs []*onebot.Message) []*onebot.Message {
	copyOf := append([]*onebot.Message(nil), msgs...)
	sort.SliceStable(copyOf, func(i, j int) bool { return copyOf[i].Time.Before(copyOf[j].Time) })
	return copyOf
}

func renderPromptMessageLine(msg *onebot.Message, rendered, peerName string) string {
	if msg == nil || rendered == "" {
		return rendered
	}
	matches := messageLinePattern.FindStringSubmatch(rendered)
	if len(matches) != 5 {
		return rendered
	}
	speaker := strings.TrimSpace(matches[3])
	if msg.UserID != 0 && strings.HasPrefix(speaker, "对方(") && peerName != "" {
		speaker = fmt.Sprintf("对方(%s,%d)", peerName, msg.UserID)
	}
	return fmt.Sprintf("[%s] #%d %s: %s\n", matches[1], msg.MessageID, speaker, strings.TrimLeft(matches[4], " "))
}
