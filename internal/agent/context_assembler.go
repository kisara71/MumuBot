package agent

import (
	"context"
	"fmt"
	"mumu-bot/internal/config"
	"mumu-bot/internal/jargon"
	"mumu-bot/internal/memory"
	"mumu-bot/internal/onebot"
	"mumu-bot/internal/prompt"
	"mumu-bot/internal/session"
	"mumu-bot/internal/tools"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/compose"
	flowagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

type AssembledContext struct {
	PromptContext *prompt.Context
	ChatContext   string
	RecentPeople  string
	ExtraPrompt   string
}

type ContextAssembler struct {
	rootCtx         context.Context
	memory          *memory.Manager
	bot             *onebot.Client
	jargonMgr       *jargon.Manager
	styleClassifier *react.Agent
}

func NewContextAssembler(rootCtx context.Context, memoryMgr *memory.Manager, bot *onebot.Client, jargonMgr *jargon.Manager, styleClassifier *react.Agent) *ContextAssembler {
	return &ContextAssembler{
		rootCtx:         rootCtx,
		memory:          memoryMgr,
		bot:             bot,
		jargonMgr:       jargonMgr,
		styleClassifier: styleClassifier,
	}
}

func (a *ContextAssembler) Assemble(ctx context.Context, sess *session.Session[*onebot.Message], lastProcessedTime time.Time, fromLoop bool) *AssembledContext {
	if sess == nil {
		return nil
	}
	ref := sess.Ref
	chatContext := assembleChatContext(sess, lastProcessedTime)
	if chatContext == "" {
		return nil
	}

	result := &AssembledContext{
		PromptContext: &prompt.Context{Ref: ref},
		ChatContext:   chatContext,
		ExtraPrompt:   resolveExtraPrompt(ref),
	}
	if fromLoop {
		result.PromptContext.LoopInfo = a.buildLoopContext(sess)
	}
	if config.Get().Agent.EnableActiveRetrieval {
		result.PromptContext.RelatedMemories, result.PromptContext.CrossGroupExperiences = a.buildMemoryContext(ctx, sess)
	}
	if targetUserID := a.resolveMoodTargetUserID(sess); targetUserID > 0 {
		if mood, err := a.memory.GetMoodState(targetUserID); err == nil {
			result.PromptContext.MoodState = &prompt.MoodInfo{
				Valence:     mood.Valence,
				Energy:      mood.Energy,
				Sociability: mood.Sociability,
				Irritation:  mood.Irritation,
				Curiosity:   mood.Curiosity,
			}
		}
	}
	if a.jargonMgr != nil {
		result.PromptContext.JargonMatches = a.jargonMgr.Match(chatContext)
	}
	if ref.IsGroup() {
		result.PromptContext.GroupInfo = a.buildGroupContext(sess)
		result.PromptContext.StyleHints = a.buildStyleHintContext(ctx, sess)
		result.RecentPeople = a.buildRecentPeopleContext(sess)
	} else if ref.IsPrivate() {
		result.PromptContext.PeerInfo = a.buildPrivatePeerContext(sess)
	}
	return result
}

func resolveExtraPrompt(ref session.Ref) string {
	if ref.IsGroup() {
		if gc := config.Get().GetGroupConfig(ref.GroupID); gc != nil {
			return gc.ExtraPrompt
		}
		return ""
	}
	if ref.IsPrivate() {
		if uc := config.Get().GetUserConfig(ref.UserID); uc != nil {
			return uc.ExtraPrompt
		}
	}
	return ""
}

func (a *ContextAssembler) buildLoopContext(sess *session.Session[*onebot.Message]) string {
	msgs := sess.Messages()
	if len(msgs) == 0 {
		return ""
	}

	now := time.Now()
	lastOther := time.Time{}
	lastSelf := time.Time{}
	selfID := int64(0)
	if a.bot != nil {
		selfID = a.bot.GetSelfID()
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if lastSelf.IsZero() && msg.UserID == selfID {
			lastSelf = msg.Time
		}
		if lastOther.IsZero() && msg.UserID != selfID {
			lastOther = msg.Time
		}
		if !lastSelf.IsZero() && !lastOther.IsZero() {
			break
		}
	}

	lines := []string{"- 这是一次定时主动思考，不是对方刚发来新消息"}
	if !lastOther.IsZero() {
		lines = append(lines, fmt.Sprintf("- 距离对方上次发言：%s", formatElapsed(now.Sub(lastOther))))
	}
	if !lastSelf.IsZero() {
		lines = append(lines, fmt.Sprintf("- 距离你上次发言：%s", formatElapsed(now.Sub(lastSelf))))
	}
	return strings.Join(lines, "\n")
}

func formatElapsed(d time.Duration) string {
	if d < time.Minute {
		return "不到1分钟"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分钟", int(d/time.Minute))
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	if m == 0 {
		return fmt.Sprintf("%d小时", h)
	}
	return fmt.Sprintf("%d小时%d分钟", h, m)
}

func (a *ContextAssembler) buildGroupContext(sess *session.Session[*onebot.Message]) string {
	ref := sess.Ref
	if a.bot == nil || !ref.IsGroup() {
		return ""
	}

	ctx, cancel := context.WithTimeout(a.rootCtx, 10*time.Second)
	defer cancel()
	info, err := a.bot.GetGroupInfo(ctx, ref.GroupID, false)
	if err != nil {
		zap.L().Debug("获取群基础信息失败", zap.Int64("group_id", ref.GroupID), zap.Error(err))
		return ""
	}
	if info == nil {
		return ""
	}

	var parts []string
	if info.GroupName != "" {
		parts = append(parts, fmt.Sprintf("- 群名: %s", info.GroupName))
	}
	if info.MaxMemberCount > 0 {
		parts = append(parts, fmt.Sprintf("- 群人数: %d/%d", info.MemberCount, info.MaxMemberCount))
	} else if info.MemberCount > 0 {
		parts = append(parts, fmt.Sprintf("- 群人数: %d", info.MemberCount))
	}
	return strings.Join(parts, "\n")
}

func (a *ContextAssembler) buildPrivatePeerContext(sess *session.Session[*onebot.Message]) string {
	ref := sess.Ref
	if !ref.IsPrivate() {
		return ""
	}

	msgs := sess.Messages()
	nickname := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].UserID == ref.UserID && msgs[i].Nickname != "" {
			nickname = msgs[i].Nickname
			break
		}
	}

	profile, err := a.memory.GetMemberProfile(ref.UserID)
	if err != nil {
		if nickname == "" {
			nickname = fmt.Sprintf("%d", ref.UserID)
		}
		return fmt.Sprintf("- 对方: %s\n- 当前场景: 这是你和对方的一对一私聊", nickname)
	}

	displayName := profile.Nickname
	if displayName == "" {
		displayName = nickname
	}
	if displayName == "" {
		displayName = fmt.Sprintf("%d", ref.UserID)
	}

	details := []string{
		fmt.Sprintf("- 对方: %s", displayName),
		fmt.Sprintf("- 亲密度: %.2f", profile.Intimacy),
		fmt.Sprintf("- 信任度: %.2f", profile.Trust),
		fmt.Sprintf("- 熟悉度: %.2f", profile.Familiarity),
		fmt.Sprintf("- 认可度: %.2f", profile.Respect),
		fmt.Sprintf("- 活跃度: %.2f", profile.Activity),
		"- 当前场景: 这是你和对方的一对一私聊",
	}
	if profile.SpeakStyle != "" {
		details = append(details, "- 说话风格: "+profile.SpeakStyle)
	}
	interests := strings.TrimSpace(profile.Interests)
	if interests != "" {
		var items []string
		if err := sonic.UnmarshalString(interests, &items); err == nil && len(items) > 0 {
			interests = strings.Join(items, "、")
		}
	}
	if interests != "" {
		details = append(details, "- 兴趣: "+interests)
	}
	return strings.Join(details, "\n")
}

func (a *ContextAssembler) resolveMoodTargetUserID(sess *session.Session[*onebot.Message]) int64 {
	ref := sess.Ref
	if ref.IsPrivate() {
		return ref.UserID
	}

	msgs := sess.Messages()
	selfID := a.bot.GetSelfID()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].UserID != 0 && msgs[i].UserID != selfID {
			return msgs[i].UserID
		}
	}
	return 0
}

func (a *ContextAssembler) buildMemoryContext(ctx context.Context, sess *session.Session[*onebot.Message]) ([]memory.Memory, []memory.Memory) {
	ref := sess.Ref
	msgs := sess.Messages()
	if len(msgs) == 0 {
		return nil, nil
	}

	query := assemblerCollectTextContext(msgs)
	if query == "" {
		return nil, nil
	}

	const threshold = 0.7
	local, err := a.memory.SearchSimilarMemoriesByConversation(ctx, query, ref, "", 4, threshold)
	if err != nil {
		zap.L().Warn("主动记忆检索失败", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Error(err))
		return nil, nil
	}

	crossLimit := 0
	switch {
	case len(local) == 0:
		crossLimit = 2
	case len(local) == 1:
		crossLimit = 1
	}
	if crossLimit == 0 {
		return local, nil
	}

	cross, err := a.memory.SearchSimilarMemoriesByConversation(ctx, query, session.AllConversationRef(), memory.MemoryTypeSelfExperience, 4, threshold)
	if err != nil {
		zap.L().Warn("跨会话自我经历检索失败", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Error(err))
		return local, nil
	}

	seen := make(map[uint]struct{}, len(local))
	for _, mem := range local {
		seen[mem.ID] = struct{}{}
	}

	result := make([]memory.Memory, 0, crossLimit)
	for _, mem := range cross {
		if crossLimit <= 0 {
			break
		}
		if _, ok := seen[mem.ID]; ok {
			continue
		}
		seen[mem.ID] = struct{}{}
		result = append(result, mem)
		if len(result) >= crossLimit {
			break
		}
	}
	return local, result
}

func assemblerCollectTextContext(msgs []*onebot.Message) string {
	msgs = assemblerSortedMessages(msgs)
	if len(msgs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

func assemblerSortedMessages(msgs []*onebot.Message) []*onebot.Message {
	if len(msgs) <= 1 {
		return msgs
	}
	cloned := append([]*onebot.Message(nil), msgs...)
	sort.SliceStable(cloned, func(i, j int) bool {
		return cloned[i].Time.Before(cloned[j].Time)
	})
	return cloned
}

func (a *ContextAssembler) buildStyleHintContext(ctx context.Context, sess *session.Session[*onebot.Message]) []string {
	ref := sess.Ref
	if !ref.IsGroup() {
		return nil
	}
	classification, err := a.classifyStyleContext(ctx, sess)
	if err != nil || classification == nil {
		if err != nil {
			zap.L().Debug("群风格分类失败", zap.Int64("group_id", ref.GroupID), zap.Error(err))
		}
		return nil
	}

	cards, err := a.memory.ListActiveStyleCardsByIntent(classification.Intent, ref.GroupID, classification.Tone, 3)
	if err != nil {
		zap.L().Warn("查询风格卡片失败", zap.Int64("group_id", ref.GroupID), zap.Error(err))
		return nil
	}
	if len(cards) == 0 {
		return nil
	}

	hints := assemblerBuildStyleHints(classification.Intent, cards)
	usedIDs := make([]uint, 0, len(cards))
	for _, card := range cards {
		usedIDs = append(usedIDs, card.ID)
	}
	if err := a.memory.IncrementStyleCardUsage(usedIDs); err != nil {
		zap.L().Debug("更新风格卡片使用计数失败", zap.Int64("group_id", ref.GroupID), zap.Error(err))
	}
	return hints
}

func (a *ContextAssembler) classifyStyleContext(ctx context.Context, sess *session.Session[*onebot.Message]) (*tools.StyleClassification, error) {
	ref := sess.Ref
	if !ref.IsGroup() {
		return nil, fmt.Errorf("私聊不需要群风格分类")
	}
	if a.styleClassifier == nil {
		return nil, fmt.Errorf("分类 Agent 未初始化")
	}

	contextText := assemblerCollectTextContext(sess.Messages())
	if contextText == "" {
		return nil, fmt.Errorf("没有可分类的文字消息")
	}

	systemPrompt := fmt.Sprintf("你负责给QQ群聊天上下文做风格分类。你必须调用一次 %s 工具提交结果，不要输出普通文本。intent 只能是：%s。tone 只能是：%s。",
		tools.StyleClassificationToolName,
		strings.Join(memory.StyleIntentValues(), "、"),
		strings.Join(memory.StyleToneValues(), "、"),
	)
	userPrompt := fmt.Sprintf("请根据下面的聊天内容，判断更适合参考的群聊风格标签，并调用工具提交。\n聊天内容：\n%s", contextText)

	result := &tools.StyleClassification{}
	classifyCtx := tools.WithStyleClassificationTarget(ctx, result)
	classifyCtx, cancel := context.WithTimeout(classifyCtx, styleClassificationTimeout)
	defer cancel()

	styleOptions := []flowagent.AgentOption{
		flowagent.WithComposeOptions(
			compose.WithChatModelOption(model.WithToolChoice(schema.ToolChoiceForced, tools.StyleClassificationToolName)),
		),
	}
	if cfg := config.Get(); cfg != nil && cfg.Debug.ShowToolCalls {
		styleOptions = append(styleOptions, flowagent.WithComposeOptions(compose.WithCallbacks(tools.NewToolLogHandler())))
	}

	_, err := a.styleClassifier.Generate(classifyCtx, []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(userPrompt),
	}, styleOptions...)
	if err != nil {
		return nil, err
	}
	if result.Intent == "" || result.Tone == "" {
		return nil, fmt.Errorf("分类工具未返回结果")
	}
	return result, nil
}

func assemblerBuildStyleHints(intent string, cards []memory.StyleCard) []string {
	hints := make([]string, 0, len(cards)+1)
	hints = append(hints, "当前推荐发言方向："+intent)
	for _, card := range cards {
		hints = append(hints, assemblerFormatStyleHint(card))
	}
	return hints
}

func assemblerFormatStyleHint(card memory.StyleCard) string {
	hint := fmt.Sprintf(
		"想说得%s一点时，可在%s的时候像“%s”这样接话，但%s时别这么说",
		card.Tone,
		card.TriggerRule,
		card.Example,
		card.AvoidRule,
	)
	if strings.TrimSpace(card.SourceExcerpt) == "" {
		return hint
	}
	rawItems := strings.Split(card.SourceExcerpt, "|")
	sourceItems := make([]string, 0, len(rawItems))
	for _, item := range rawItems {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		sourceItems = append(sourceItems, item)
	}
	if len(sourceItems) == 0 {
		return hint
	}
	return hint + "，可参考群里人说过的原话：" + strings.Join(sourceItems, " / ")
}

func assembleChatContext(sess *session.Session[*onebot.Message], lastProcessedTime time.Time) string {
	msgs := assemblerSortedMessages(sess.Messages())
	if len(msgs) == 0 {
		return ""
	}

	var b strings.Builder
	for _, m := range msgs {
		if !lastProcessedTime.IsZero() && m.Time.Before(lastProcessedTime) {
			b.WriteString("(OLD)")
		}
		b.WriteString(m.FinalContent)
	}
	return b.String()
}

func (a *ContextAssembler) buildRecentPeopleContext(sess *session.Session[*onebot.Message]) string {
	msgs := sess.Messages()
	if len(msgs) == 0 {
		return ""
	}

	seenIDs := make(map[int64]struct{}, 3)
	ids := make([]int64, 0, 3)
	for i := len(msgs) - 1; i >= 0; i-- {
		userID := msgs[i].UserID
		if userID == 0 || userID == a.bot.GetSelfID() {
			continue
		}
		if _, ok := seenIDs[userID]; ok {
			continue
		}
		seenIDs[userID] = struct{}{}
		ids = append(ids, userID)
		if len(ids) >= 3 {
			break
		}
	}
	if len(ids) == 0 {
		return ""
	}

	latestNicknames := make(map[int64]string, len(ids))
	for i := len(msgs) - 1; i >= 0; i-- {
		if _, ok := latestNicknames[msgs[i].UserID]; ok {
			continue
		}
		latestNicknames[msgs[i].UserID] = msgs[i].Nickname
	}

	lines := make([]string, 0, len(ids))
	for _, userID := range ids {
		nickname := latestNicknames[userID]
		profile, err := a.memory.GetMemberProfile(userID)
		if err != nil {
			if nickname == "" {
				nickname = fmt.Sprintf("%d", userID)
			}
			lines = append(lines, fmt.Sprintf("- %s：最近在场。", nickname))
			continue
		}

		displayName := profile.Nickname
		if displayName == "" {
			displayName = nickname
		}
		if displayName == "" {
			displayName = fmt.Sprintf("%d", userID)
		}

		details := []string{
			fmt.Sprintf("亲密度 %.2f", profile.Intimacy),
			fmt.Sprintf("信任度 %.2f", profile.Trust),
			fmt.Sprintf("熟悉度 %.2f", profile.Familiarity),
			fmt.Sprintf("认可度 %.2f", profile.Respect),
			fmt.Sprintf("活跃度 %.2f", profile.Activity),
		}
		if profile.SpeakStyle != "" {
			details = append(details, "风格: "+profile.SpeakStyle)
		}
		lines = append(lines, fmt.Sprintf("- %s：%s", displayName, strings.Join(details, "；")))
	}
	return strings.Join(lines, "\n")
}
