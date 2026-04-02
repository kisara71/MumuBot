package learning

import (
	"context"
	"fmt"
	"mumu-bot/internal/config"
	"mumu-bot/internal/jargon"
	"mumu-bot/internal/llm"
	"mumu-bot/internal/memory"
	"mumu-bot/internal/tools"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	agentflow "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

//	TODO

type Learner struct {
	memMgr    *memory.Manager
	jargonMgr *jargon.Manager
	auxModel  model.ToolCallingChatModel
	tools     []tool.BaseTool
	agent     *react.Agent
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	isRunning bool
	mu        sync.Mutex
}

func New(memMgr *memory.Manager, jargonMgr *jargon.Manager) (*Learner, error) {
	auxModel, err := llm.NewAuxClient()
	if err != nil {
		return nil, err
	}

	var learningTools []tool.BaseTool
	toolBuilders := []func() (tool.BaseTool, error){
		func() (tool.BaseTool, error) { return tools.NewSaveJargonTool() },
		func() (tool.BaseTool, error) { return tools.NewSaveStyleCardTool() },
		func() (tool.BaseTool, error) { return tools.NewGetUncheckedStyleCardsTool() },
		func() (tool.BaseTool, error) { return tools.NewReviewStyleCardTool() },
		func() (tool.BaseTool, error) { return tools.NewGetUncheckedJargonsTool() },
		func() (tool.BaseTool, error) { return tools.NewReviewJargonTool() },
	}
	for _, build := range toolBuilders {
		t, err := build()
		if err != nil {
			return nil, err
		}
		learningTools = append(learningTools, t)
	}

	// Initialize ReAct agent
	cfg := config.Get()
	maxStep := cfg.Learning.MaxStep
	if maxStep <= 0 {
		maxStep = 10
	}
	agent, err := react.NewAgent(context.Background(), &react.AgentConfig{
		ToolCallingModel: auxModel,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools: learningTools,
			ToolArgumentsHandler: func(ctx context.Context, name, arguments string) (string, error) {
				return tools.CanonicalizeToolArguments(arguments)
			},
			ToolCallMiddlewares: []compose.ToolMiddleware{{
				Invokable: tools.ToolDedupMiddleware(),
			}},
		},
		MaxStep: maxStep,
	})
	if err != nil {
		return nil, fmt.Errorf("创建学习 Agent 失败: %w", err)
	}

	l := &Learner{
		memMgr:    memMgr,
		jargonMgr: jargonMgr,
		auxModel:  auxModel,
		tools:     learningTools,
		agent:     agent,
	}

	return l, nil
}

func (l *Learner) Start(parent context.Context) {
	l.mu.Lock()
	if l.isRunning {
		l.mu.Unlock()
		return
	}

	if parent == nil {
		parent = context.Background()
	}

	l.ctx, l.cancel = context.WithCancel(parent)
	l.isRunning = true
	l.mu.Unlock()

	l.wg.Add(1)
	go l.runLoop()
	zap.L().Info("后台学习系统已启动")
}

func (l *Learner) Stop() {
	l.mu.Lock()
	if !l.isRunning {
		l.mu.Unlock()
		return
	}

	cancel := l.cancel
	l.isRunning = false
	l.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	l.wg.Wait()
	zap.L().Info("后台学习系统已停止")
}

func (l *Learner) runLoop() {
	defer l.wg.Done()
	cfg := config.Get()
	// Check interval
	intervalMinutes := cfg.Learning.IntervalMinutes
	if intervalMinutes <= 0 {
		intervalMinutes = 10
	}
	interval := time.Duration(intervalMinutes) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	l.runTask()

	// 启动定期审核任务
	reviewIntervalMinutes := cfg.Learning.ReviewIntervalMinutes
	if reviewIntervalMinutes <= 0 {
		reviewIntervalMinutes = 30
	}
	reviewTicker := time.NewTicker(time.Duration(reviewIntervalMinutes) * time.Minute)
	defer reviewTicker.Stop()

	for {
		select {
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			l.runTask()
		case <-reviewTicker.C:
			l.runReviewTask()
		}
	}
}

func (l *Learner) runTask() {
	cfg := config.Get()
	for _, group := range cfg.Groups {
		if !group.Enabled {
			continue
		}
		if err := l.ctx.Err(); err != nil {
			return
		}
		l.processGroup(group.GroupID)
	}
}

func (l *Learner) runReviewTask() {
	cfg := config.Get()
	for _, group := range cfg.Groups {
		if !group.Enabled {
			continue
		}
		if err := l.ctx.Err(); err != nil {
			return
		}
		l.processReview(group.GroupID)
	}
}

func (l *Learner) processReview(groupID int64) {
	prompt := `请检查当前待审核的“黑话/梗”和“群聊风格卡片”。
你需要使用 'getUncheckedJargons' 和 'getUncheckedStyleCards' 工具来获取待审核列表。
然后，根据你的知识库判断这些内容的准确性和健康度。
- 如果内容准确且无害，使用 'reviewJargon' 或 'reviewStyleCard' 通过审核 (approve=true)。
- 如果内容明显错误、垃圾信息或有害，请拒绝 (approve=false)。
- 如果你不确定，请保持待审核状态（不做操作）。

审核风格卡片时重点看：
1. 这是不是群里可复用的说话风格，而不是具体事件内容；
2. trigger_rule 和 avoid_rule 是否明确；
3. 是否容易误伤、过度攻击或强烈阴阳；
4. 例句是否短、自然、像参考味道而不是模板。

注意：审核工具支持批量操作，请将同一审核结果的 ID 放入列表中一次性提交，尽量减少工具调用次数。`

	// 创建学习上下文
	ctx, cancel := context.WithTimeout(l.ctx, 60*time.Second)
	defer cancel()
	ctx = tools.WithLearningContext(ctx, &tools.LearningContext{
		ConversationRef: memory.GroupConversationRef(groupID),
		MemMgr:          l.memMgr,
		JargonMgr:       l.jargonMgr,
	})

	// 调用 Agent
	opts := []agentflow.AgentOption{}
	if cfg := config.Get(); cfg != nil && cfg.Debug.ShowToolCalls {
		opts = append(opts, agentflow.WithComposeOptions(compose.WithCallbacks(tools.NewToolLogHandler())))
	}
	_, err := l.agent.Generate(ctx, []*schema.Message{
		schema.UserMessage(prompt),
	}, opts...)
	if err != nil {
		zap.L().Error("后台审核任务失败", zap.Int64("group_id", groupID), zap.Error(err))
		return
	}

	zap.L().Info("后台审核任务完成", zap.Int64("group_id", groupID))
}

func (l *Learner) processGroup(groupID int64) {
	// 获取上次学习进度
	state, err := l.memMgr.GetLearningState(groupID)
	if err != nil {
		zap.L().Error("获取学习进度失败", zap.Int64("group_id", groupID), zap.Error(err))
		return
	}

	// Fetch messages after last learned ID (limit batchSize)
	cfg := config.Get()
	batchSize := cfg.Learning.BatchSize
	if batchSize <= 0 {
		batchSize = 100
	}

	msgs, err := l.memMgr.GetMessagesAfterID(memory.GroupConversationRef(groupID), cfg.Persona.QQ, state.LastMessageID, batchSize)
	if err != nil {
		zap.L().Error("获取消息失败", zap.Int64("group_id", groupID), zap.Error(err))
		return
	}

	if len(msgs) == 0 {
		return
	}

	// 记录本批次最新的消息ID，用于更新进度
	newLastID := state.LastMessageID
	if len(msgs) > 0 {
		newLastID = msgs[len(msgs)-1].ID
	}

	minMsgCount := cfg.Learning.MinMsgCount
	if minMsgCount <= 0 {
		minMsgCount = 5
	}
	if len(msgs) < minMsgCount {
		// 消息太少，直接跳过，也不更新进度，等待积累更多
		return
	}

	// 构建提示词
	var chatLog strings.Builder
	for _, m := range msgs {
		if m.OriginalContent == "" {
			continue
		}
		chatLog.WriteString(fmt.Sprintf("%s: %s\n", m.Nickname, m.OriginalContent))
	}

	// 注入已知的黑话/梗（避免重复学习）
	knownJargons := ""
	if l.jargonMgr != nil {
		matches := l.jargonMgr.Match(chatLog.String())
		if len(matches) > 0 {
			var b strings.Builder
			b.WriteString("\n【已知黑话】：\n")
			for term, meaning := range matches {
				b.WriteString(fmt.Sprintf("- %s: %s\n", term, meaning))
			}
			knownJargons = b.String()
		}
	}

	prompt := fmt.Sprintf(`请分析以下 QQ 群聊天记录。你的任务是提取“黑话/梗”（该群体特有的术语、缩写、meme）和“群聊风格卡片”（这个群在特定场景下常见的说话方式）。

聊天记录：

%s

%s

要求：
1. 识别**新的**黑话/梗。黑话的含义应该是与上下文相关的，如果无需上下文就可以推断该词的意思，则该词不是黑话。
2. 识别可复用的群聊风格卡片，而不是一次性内容。
3. 风格卡片必须使用以下 intent 枚举之一：%s。
4. 风格卡片必须使用以下 tone 枚举之一：%s。
5. 每张风格卡片必须包含：intent、tone、trigger_rule、avoid_rule、example、source_excerpt。
6. example 必须是短句，只作为语气味道参考，不能写成长模板。
7. 如果无法给出明确的 trigger_rule 和 avoid_rule，就不要保存该卡片。
8. 强攻击性、强冒犯性、强阴阳且容易误伤的表达不要保存为风格卡片。
9. 忽略通用语言或普通词汇，专注于独特的群体文化。
10. 使用提供的工具 'saveJargon' 和 'saveStyleCard' 来保存你的发现。
11. 如果没有发现有价值的内容，请直接回复“无新发现”。
`, chatLog.String(), knownJargons, strings.Join(memory.StyleIntentValues(), "、"), strings.Join(memory.StyleToneValues(), "、"))

	// 创建学习上下文
	ctx, cancel := context.WithTimeout(l.ctx, 90*time.Second)
	defer cancel()
	ctx = tools.WithLearningContext(ctx, &tools.LearningContext{
		ConversationRef: memory.GroupConversationRef(groupID),
		MemMgr:          l.memMgr,
		JargonMgr:       l.jargonMgr,
	})

	// 调用 Agent
	opts := []agentflow.AgentOption{}
	if cfg := config.Get(); cfg != nil && cfg.Debug.ShowToolCalls {
		opts = append(opts, agentflow.WithComposeOptions(compose.WithCallbacks(tools.NewToolLogHandler())))
	}
	_, err = l.agent.Generate(ctx, []*schema.Message{
		schema.UserMessage(prompt),
	}, opts...)
	if err != nil {
		zap.L().Error("后台学习任务失败", zap.Int64("group_id", groupID), zap.Error(err))
		return
	}

	zap.L().Info("后台学习任务完成", zap.Int64("group_id", groupID))

	// 更新进度
	if err := l.memMgr.UpdateLearningState(groupID, newLastID); err != nil {
		zap.L().Error("更新学习进度失败", zap.Int64("group_id", groupID), zap.Error(err))
	}
}
