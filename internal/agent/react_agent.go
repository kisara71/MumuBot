package agent

import (
	"context"
	"errors"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/llm"
	"github.com/kisara71/luma/internal/mcp"
	"github.com/kisara71/luma/internal/memory"
	"github.com/kisara71/luma/internal/onebot"
	"github.com/kisara71/luma/internal/prompt"
	"github.com/kisara71/luma/internal/session"
	"github.com/kisara71/luma/internal/tools"
	"github.com/kisara71/luma/internal/utils"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	flowagent "github.com/cloudwego/eino/flow/agent"
	"github.com/cloudwego/eino/flow/agent/react"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

const (
	agentThinkTimeout = 60 * time.Second
)

// Agent 私聊伴侣智能体。
type Agent struct {
	ctx              context.Context
	cancel           context.CancelFunc
	personaName      string
	memory           *memory.Manager
	model            model.ToolCallingChatModel
	vision           *llm.VisionClient // 多模态视觉模型
	bot              *onebot.Client
	react            *react.Agent
	tools            []tool.BaseTool
	mcpMgr           *mcp.Manager        // MCP 管理器
	concurrencyMgr   *ConcurrencyManager // 并发管理器
	contextAssembler *ContextAssembler
	promptBuilder    *prompt.Builder

	// 会话运行时
	sessions   map[string]*session.Session[*onebot.Message]
	sessionsMu sync.RWMutex

	lifecycleMu sync.Mutex
	started     bool
	stopped     bool
	wg          sync.WaitGroup
}

func messageConversationRef(msg *onebot.Message) session.Ref {
	if msg == nil {
		return session.Ref{}
	}
	if ref, ok := session.ParseRefID(msg.ConversationID); ok {
		return ref
	}
	return session.Ref{}
}

// New 创建 Agent
func New(mem *memory.Manager) (*Agent, error) {
	cfg := config.Get()
	if cfg == nil {
		return nil, fmt.Errorf("配置未加载")
	}

	chatModel, err := llm.NewClient()
	if err != nil {
		return nil, fmt.Errorf("创建 LLM 客户端失败: %w", err)
	}
	zap.L().Info("LLM 已连接", zap.String("model", cfg.LLM.Model), zap.String("base_url", cfg.LLM.BaseURL))

	var visionClient *llm.VisionClient
	if cfg.VisionLLM.Enabled {
		visionClient, err = llm.NewVisionClient()
		if err != nil {
			zap.L().Error("Vision 客户端创建失败，视觉理解不可用", zap.Error(err))
		} else if visionClient != nil {
			zap.L().Info("Vision 已启用", zap.String("model", cfg.VisionLLM.Model))
		}
	}

	botClient := onebot.NewClient()

	rootCtx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		ctx:           rootCtx,
		cancel:        cancel,
		personaName:   cfg.Persona.Name,
		memory:        mem,
		model:         chatModel,
		vision:        visionClient,
		bot:           botClient,
		promptBuilder: prompt.NewBuilder(&cfg.Persona),
		sessions:      make(map[string]*session.Session[*onebot.Message]),
	}

	zap.L().Info("人格已加载", zap.String("name", a.personaName))

	// 初始化并发管理器
	a.concurrencyMgr = NewConcurrencyManager(a.ctx, cfg.Agent.MaxCoroutine, a.think)

	// 初始化 MCP 管理器
	a.mcpMgr = mcp.NewMCPManager()
	if err := a.mcpMgr.LoadFromConfig(a.ctx, "config/mcp.json"); err != nil {
		zap.L().Error("加载 MCP 配置失败", zap.Error(err))
	}

	if err := a.initTools(); err != nil {
		a.mcpMgr.Close()
		a.cancel()
		return nil, err
	}
	if err := a.initReact(); err != nil {
		a.mcpMgr.Close()
		a.cancel()
		return nil, err
	}
	a.contextAssembler = NewContextAssembler(a.memory, a.bot)
	return a, nil
}

func (a *Agent) initTools() error {
	toolBuilders := []func() (tool.BaseTool, error){
		// 记忆相关
		func() (tool.BaseTool, error) { return tools.NewSaveMemoryTool() },
		func() (tool.BaseTool, error) { return tools.NewQueryMemoryTool() },
		// 用户信息
		func() (tool.BaseTool, error) { return tools.NewUpdateUserProfileTool() },
		func() (tool.BaseTool, error) { return tools.NewGetUserInfoTool() },
		func() (tool.BaseTool, error) { return tools.NewGetRecentMessagesTool() },
		// 发言相关
		func() (tool.BaseTool, error) { return tools.NewSpeakTool() },
		func() (tool.BaseTool, error) { return tools.NewStayQuietTool() },
		// 表情包相关
		func() (tool.BaseTool, error) { return tools.NewSearchStickersTool() },
		func() (tool.BaseTool, error) { return tools.NewSendStickerTool() },
		func() (tool.BaseTool, error) { return tools.NewGetForwardMessageDetailTool() },
		// 情绪系统
		func() (tool.BaseTool, error) { return tools.NewUpdateMoodTool() },
	}

	seen := make(map[string]struct{}, len(toolBuilders))
	addTool := func(t tool.BaseTool, source string) error {
		info, err := t.Info(a.ctx)
		if err != nil {
			return fmt.Errorf("读取%s工具信息失败: %w", source, err)
		}
		if info == nil || strings.TrimSpace(info.Name) == "" {
			return fmt.Errorf("%s工具缺少名称", source)
		}
		name := strings.TrimSpace(info.Name)
		if _, exists := seen[name]; exists {
			return fmt.Errorf("工具名称冲突: %s", name)
		}
		seen[name] = struct{}{}
		a.tools = append(a.tools, t)
		return nil
	}

	for _, build := range toolBuilders {
		t, err := build()
		if err != nil {
			return err
		}
		if err := addTool(t, "内置"); err != nil {
			return err
		}
	}

	// 添加 MCP 工具
	mcpTools := a.mcpMgr.GetTools()
	loaded := 0
	for _, t := range mcpTools {
		if err := addTool(t, "MCP"); err != nil {
			zap.L().Warn("跳过不可用的 MCP 工具", zap.Error(err))
			continue
		}
		loaded++
	}
	if loaded > 0 {
		zap.L().Info("已加载 MCP 工具", zap.Int("count", loaded))
	}

	return nil
}

func (a *Agent) initReact() error {
	cfg := config.Get()
	maxStep := cfg.Agent.MaxStep
	if maxStep <= 0 {
		maxStep = 6
	}
	agent, err := react.NewAgent(a.ctx, &react.AgentConfig{
		ToolCallingModel: a.model,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools:               a.tools,
			ExecuteSequentially: true,
			ToolArgumentsHandler: func(ctx context.Context, name, arguments string) (string, error) {
				return tools.CanonicalizeToolArguments(arguments)
			},
			ToolCallMiddlewares: []compose.ToolMiddleware{{
				Invokable: tools.ToolDedupMiddleware(),
			}},
		},
		MaxStep: maxStep,
		// A chat turn ends after exactly one visible action. This is a hard
		// behavioural boundary and avoids multi-message ReAct runs.
		ToolReturnDirectly: map[string]struct{}{
			"speak": {}, "sendSticker": {}, "stayQuiet": {},
		},
	})
	if err != nil {
		return err
	}
	a.react = agent
	return nil
}

// Start 启动
func (a *Agent) Start() error {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.stopped {
		return fmt.Errorf("Agent 已停止")
	}
	if a.started {
		return nil
	}

	// 启动时从数据库加载历史消息到缓冲区
	a.loadBuffersFromDB()

	a.bot.OnMessage(a.onMessage)
	if err := a.bot.Connect(); err != nil {
		return fmt.Errorf("OneBot 连接失败: %w", err)
	}
	a.wg.Add(1)
	go a.thinkLoop()
	a.started = true
	zap.L().Info("Agent 已启动")
	return nil
}

// loadBuffersFromDB 从数据库加载消息日志到缓冲区
func (a *Agent) loadBuffersFromDB() {
	cfg := config.Get()
	zap.L().Info("加载私聊历史")
	for _, uc := range cfg.Users {
		if !uc.Enabled {
			continue
		}
		logs := a.memory.GetRecentMessages(session.NewRef(uc.UserID), cfg.Agent.MessageBufferSize, 0)
		if len(logs) == 0 {
			continue
		}
		a.loadMessages(session.NewRef(uc.UserID), cfg.Agent.MessageBufferSize, logs)
		zap.L().Info(fmt.Sprintf("用户:%d加载了%d条信息", uc.UserID, len(logs)))
	}
	zap.L().Info("私聊历史加载完毕")
}

func (a *Agent) loadMessages(ref session.Ref, bufSize int, logs []memory.MessageLog) {
	if len(logs) == 0 {
		return
	}

	sess := session.NewSession[*onebot.Message](ref, a.sessionBufferSize(bufSize))
	for _, log := range logs {
		msgID, _ := strconv.ParseInt(log.MessageID, 10, 64)

		// 还原合并转发内容
		var forwards []onebot.ForwardMessage
		if log.Forwards != "" {
			_ = sonic.UnmarshalString(log.Forwards, &forwards)
		}
		msg := &onebot.Message{
			MessageID:      msgID,
			ConversationID: ref.ID(),
			UserID:         log.UserID,
			Nickname:       log.Nickname,
			Content:        log.OriginalContent,
			FinalContent:   log.Content,
			Time:           log.CreatedAt,
			Forwards:       forwards,
		}
		sess.Push(msg)
	}
	a.scheduleNextProactive(sess, time.Now())

	a.sessionsMu.Lock()
	a.sessions[ref.ID()] = sess
	a.sessionsMu.Unlock()
}

// Stop 停止
func (a *Agent) Stop() {
	a.lifecycleMu.Lock()
	if a.stopped {
		a.lifecycleMu.Unlock()
		return
	}
	a.stopped = true
	a.lifecycleMu.Unlock()

	a.cancel()
	a.clearPendingThinks()
	if a.bot != nil {
		_ = a.bot.Close()
	}
	if a.concurrencyMgr != nil {
		a.concurrencyMgr.Close()
	}
	a.wg.Wait()
	// 关闭 MCP 连接
	if a.mcpMgr != nil {
		a.mcpMgr.Close()
	}
	zap.L().Info("Agent 已停止")
}

// launchBackground serializes task admission with Stop so Wait never races
// with a late WaitGroup.Add from a message callback.
func (a *Agent) launchBackground(fn func()) bool {
	if fn == nil {
		return false
	}
	a.lifecycleMu.Lock()
	if a.stopped {
		a.lifecycleMu.Unlock()
		return false
	}
	a.wg.Add(1)
	a.lifecycleMu.Unlock()
	go func() {
		defer a.wg.Done()
		fn()
	}()
	return true
}

func (a *Agent) onMessage(msg *onebot.Message) {
	if err := a.ctx.Err(); err != nil {
		return
	}
	if !config.Get().IsUserEnabled(msg.UserID) && msg.UserID != a.bot.GetSelfID() {
		return
	}

	// 序列化合并转发内容
	forwardsJSON := ""
	if len(msg.Forwards) > 0 {
		if b, err := sonic.MarshalString(msg.Forwards); err == nil {
			forwardsJSON = b
		}
	}

	// 解析消息内容（图片、视频、表情、回复等）
	parsedContent := a.parseMessageContent(msg)
	msg.FinalContent = parsedContent

	a.addBuffer(msg)
	if err := a.memory.AddMessage(memory.MessageLog{
		MessageID:       fmt.Sprintf("%d", msg.MessageID),
		ConversationID:  messageConversationRef(msg).ID(),
		UserID:          msg.UserID,
		Nickname:        msg.Nickname,
		Content:         msg.FinalContent, // 使用解析后的内容
		OriginalContent: msg.Content,
		CreatedAt:       msg.Time,
		Forwards:        forwardsJSON,
	}); err != nil {
		zap.L().Error("保存消息记录失败", zap.String("conversation_id", messageConversationRef(msg).ID()), zap.Error(err))
	}

	if msg.UserID == a.bot.GetSelfID() {
		return
	}
	if sess := a.getSession(messageConversationRef(msg)); sess != nil {
		sess.ClearProactivePlan()
	}

	a.launchBackground(func() { a.updateUser(msg) })

	a.scheduleThink(messageConversationRef(msg), false)
}

// parseMessageContent 解析消息内容（图片、视频、表情、回复等）
func (a *Agent) parseMessageContent(msg *onebot.Message) string {
	ctx, cancel := context.WithTimeout(a.ctx, 30*time.Second)
	defer cancel()

	// 构建回复信息
	replyInfo := ""
	if msg.Reply != nil {
		if msg.Reply.Content != "" {
			replyContent := []rune(msg.Reply.Content)
			if len(replyContent) > 50 {
				replyContent = replyContent[:50]
			}
			replyInfo = fmt.Sprintf(" [回复 #%d %s:\"%s\"]", msg.Reply.MessageID, msg.Reply.Nickname, string(replyContent))
		} else {
			replyInfo = fmt.Sprintf(" [回复 #%d]", msg.Reply.MessageID)
		}
	}

	// 构建消息内容（包含图片和表情描述）
	content := msg.Content

	// 处理表情
	for _, face := range msg.Faces {
		if face.Name != "" {
			content += fmt.Sprintf(" [表情:%s]", face.Name)
		} else if face.ID > 0 {
			content += fmt.Sprintf(" [表情:%d]", face.ID)
		} else {
			content += " [表情]"
		}
	}

	// 处理图片（调用 Vision 模型识别）
	for _, img := range msg.Images {
		if img.SubType == 1 {
			// 表情包类型
			var desc string
			if a.vision != nil && img.URL != "" {
				if d, err := a.vision.DescribeImage(ctx, img.URL); err == nil {
					desc = d
				}
			}
			if desc == "" && img.Summary != "" {
				desc = img.Summary
			}
			// 自动保存表情包
			if img.URL != "" && config.Get().Sticker.AutoSave && a.ctx.Err() == nil {
				url, stickerDesc := img.URL, desc
				a.launchBackground(func() { a.autoSaveSticker(a.ctx, url, stickerDesc) })
			}
			if desc != "" {
				content += fmt.Sprintf(" [表情包:%s]", desc)
			} else {
				content += " [表情包]"
			}
		} else {
			// 普通图片
			if a.vision != nil && img.URL != "" {
				if desc, err := a.vision.DescribeImage(ctx, img.URL); err == nil {
					content += " " + desc
				} else {
					content += " [图片]"
				}
			} else {
				content += " [图片]"
			}
		}
	}

	// 处理视频（调用 Vision 模型识别）
	for _, vid := range msg.Videos {
		if a.vision != nil && vid.URL != "" {
			if desc, err := a.vision.DescribeVideo(ctx, vid.URL); err == nil {
				content += " " + desc
			} else {
				content += " [视频]"
			}
		} else {
			content += " [视频]"
		}
	}

	speaker := a.formatMessageSpeaker(msg)

	// 构建完整消息行
	return fmt.Sprintf("[%s] #%d %s:%s %s\n",
		msg.Time.Format("15:04:05"), msg.MessageID, speaker, replyInfo, content)
}

func (a *Agent) formatMessageSpeaker(msg *onebot.Message) string {
	if msg == nil {
		return "未知"
	}

	if msg.UserID == a.bot.GetSelfID() {
		if name := strings.TrimSpace(a.personaName); name != "" {
			return fmt.Sprintf("你(%s)", name)
		}
		return "你"
	}

	name := strings.TrimSpace(msg.Nickname)
	if name == "" {
		name = "未知用户"
	}
	return fmt.Sprintf("对方(%s,%d)", name, msg.UserID)
}

func (a *Agent) addBuffer(msg *onebot.Message) {
	ref := messageConversationRef(msg)
	if ref.ID() == "" {
		zap.L().Error("消息缺少有效会话信息", zap.Any("msg", msg))
		return
	}

	a.getOrCreateSession(ref, 0).Push(msg)
}

func (a *Agent) getSession(ref session.Ref) *session.Session[*onebot.Message] {
	if ref.ID() == "" {
		return nil
	}

	a.sessionsMu.RLock()
	session := a.sessions[ref.ID()]
	a.sessionsMu.RUnlock()
	return session
}

func (a *Agent) getOrCreateSession(ref session.Ref, capacity int) *session.Session[*onebot.Message] {
	if ref.ID() == "" {
		return nil
	}
	if session := a.getSession(ref); session != nil {
		return session
	}

	a.sessionsMu.Lock()
	defer a.sessionsMu.Unlock()

	if session := a.sessions[ref.ID()]; session != nil {
		return session
	}

	sess := session.NewSession[*onebot.Message](ref, a.sessionBufferSize(capacity))
	a.sessions[ref.ID()] = sess
	return sess
}

func (a *Agent) sessionBufferSize(fallback int) int {
	if fallback > 0 {
		return fallback
	}

	if size := config.Get().Agent.MessageBufferSize; size > 0 {
		return size
	}
	return 40
}

func (a *Agent) updateUser(msg *onebot.Message) {
	if err := a.ctx.Err(); err != nil {
		return
	}
	if err := a.memory.RecordUserMessage(msg.UserID, msg.Nickname, msg.Time); err != nil {
		zap.L().Error("更新用户画像失败", zap.Error(err))
	}
}

func (a *Agent) thinkLoop() {
	defer a.wg.Done()
	interval := time.Duration(config.Get().Agent.ProactiveCheckInterval) * time.Second
	zap.L().Info("主动联系调度器已启动", zap.Duration("interval", interval), zap.Int("users", len(config.Get().Users)))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-a.ctx.Done():
			return
		case <-ticker.C:
			a.thinkCycle()
		}
	}
}

func (a *Agent) thinkCycle() {
	cfg := config.Get()
	if !cfg.Chat.Proactive.Enabled || inQuietHours(time.Now(), cfg.Chat.Proactive) {
		return
	}
	for _, uc := range cfg.Users {
		if !uc.Enabled {
			continue
		}
		ref := session.NewRef(uc.UserID)
		sess := a.getSession(ref)
		if sess == nil || sess.IsEmpty() {
			continue
		}
		if !sess.ProactiveDue(time.Now()) {
			continue
		}
		// Consume the plan before submitting. A failed/quiet generation gets a
		// fresh future plan in think(), never another try on the next tick.
		sess.ClearProactivePlan()
		a.scheduleThink(ref, true)
	}
}

func inQuietHours(now time.Time, cfg config.ProactiveConfig) bool {
	start, end := cfg.QuietStartHour, cfg.QuietEndHour
	if start == end {
		return false
	}
	hour := now.Hour()
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

func (a *Agent) scheduleNextProactive(sess *session.Session[*onebot.Message], now time.Time) {
	if sess == nil || !config.Get().Chat.Proactive.Enabled {
		return
	}
	cfg := config.Get().Chat.Proactive
	minDelay := time.Duration(cfg.MinIdleMinutes) * time.Minute
	maxDelay := time.Duration(cfg.MaxIdleMinutes) * time.Minute
	delay := minDelay
	if span := maxDelay - minDelay; span > 0 {
		delay += time.Duration(rand.Int63n(int64(span) + 1))
	}
	sess.ScheduleProactive(now.Add(delay))
}

func (a *Agent) scheduleThink(ref session.Ref, proactive bool) {
	if !ref.Valid() {
		return
	}

	debounce := time.Duration(config.Get().Agent.ThinkDebounceMS) * time.Millisecond
	session := a.getOrCreateSession(ref, 0)
	if session == nil {
		return
	}

	session.SchedulePending(debounce, proactive, func(generation uint64) {
		a.flushPendingThink(ref, generation)
	})
}

func (a *Agent) flushPendingThink(ref session.Ref, generation uint64) {
	session := a.getSession(ref)
	if session == nil {
		return
	}

	proactive, ok := session.ConsumePending(generation)
	if !ok {
		return
	}

	a.concurrencyMgr.Submit(ref, proactive)
}

func (a *Agent) clearPendingThinks() {
	a.sessionsMu.RLock()
	sessions := make([]*session.Session[*onebot.Message], 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}
	a.sessionsMu.RUnlock()

	for _, session := range sessions {
		session.ClearPending()
	}
}

// think 提交思考任务
func (a *Agent) think(ref session.Ref, proactive bool) {
	if err := a.ctx.Err(); err != nil {
		return
	}

	sess := a.getOrCreateSession(ref, 0)
	if sess == nil {
		return
	}

	if !sess.TryBeginProcessing() {
		return
	}

	defer func() {
		sess.FinishProcessing()
	}()

	ctx := tools.WithToolContext(a.ctx, &tools.ToolContext{
		Session:   sess,
		MemoryMgr: a.memory,
		Bot:       a.bot,
		SpeakCallback: func(callCtx context.Context, callRef session.Ref, messages []string) ([]int64, error) {
			return a.doSpeak(callCtx, callRef, messages)
		},
		SendStickerCallback: func(callCtx context.Context, callRef session.Ref, filePath string, description string) (int64, error) {
			return a.doSendSticker(callCtx, callRef, filePath, description)
		},
	})

	assembled := a.contextAssembler.Assemble(ctx, sess, proactive)
	if assembled == nil || assembled.PromptContext == nil || assembled.ChatContext == "" {
		return
	}
	systemPrompt := a.promptBuilder.BuildSystemPrompt()
	thinkPrompt := a.promptBuilder.BuildThinkPrompt(prompt.BuildInput{
		Context:     assembled.PromptContext,
		IsFirst:     assembled.IsFirst,
		ChatContext: assembled.ChatContext,
		ExtraPrompt: assembled.ExtraPrompt,
	})

	// 调试：显示系统提示词
	if config.Get().Debug.ShowPrompt {
		zap.L().Debug("系统提示词", zap.String("prompt", systemPrompt))
		zap.L().Debug("思考提示词", zap.String("prompt", thinkPrompt))
	}

	msgs := []*schema.Message{
		schema.SystemMessage(systemPrompt),
		schema.UserMessage(thinkPrompt),
	}

	// 设置超时时间（默认60秒），防止LLM请求无限阻塞
	ctxWithTimeout, cancelTimeout := context.WithTimeout(ctx, agentThinkTimeout)
	defer cancelTimeout()

	opts := make([]flowagent.AgentOption, 0, 1)
	if cfg := config.Get(); cfg != nil && cfg.Debug.ShowToolCalls {
		opts = append(opts, flowagent.WithComposeOptions(compose.WithCallbacks(tools.NewToolLogHandler())))
	}

	result, err := a.react.Generate(ctxWithTimeout, msgs, opts...)
	if err != nil {
		// 区分是超时还是主动取消（stayQuiet）
		if errors.Is(ctxWithTimeout.Err(), context.DeadlineExceeded) {
			zap.L().Warn("思考超时", zap.Int64("user_id", ref.UserID), zap.Duration("timeout", agentThinkTimeout))
		} else if errors.Is(ctxWithTimeout.Err(), context.Canceled) || errors.Is(a.ctx.Err(), context.Canceled) {
			zap.L().Debug("思考已取消", zap.Int64("user_id", ref.UserID))
		} else {
			zap.L().Error("思考失败", zap.Int64("user_id", ref.UserID), zap.Error(err))
		}
	}

	// 记录 Agent 输出
	if config.Get().Debug.ShowThinking && result != nil && result.Content != "" {
		zap.L().Debug("Agent 输出", zap.Int64("user_id", ref.UserID), zap.String("content", result.Content))
	}
	// Some compatible models return the intended chat text directly instead of
	// issuing speak. Treat that as a valid reply unless the model already chose
	// speak, a sticker, or silence.
	if err == nil && result != nil && !tools.GetToolContext(ctx).TerminalActionTaken() {
		content := strings.TrimSpace(result.Content)
		if content != "" {
			runes := []rune(content)
			if len(runes) > 500 {
				content = string(runes[:500])
			}
			if _, sendErr := a.doSpeak(ctxWithTimeout, ref, []string{content}); sendErr != nil {
				zap.L().Error("发送模型直接回复失败", zap.Int64("user_id", ref.UserID), zap.Error(sendErr))
			}
		}
	}
	// Every completed reflection gets another future opportunity. The model sees
	// the actual conversation and decides whether speaking is natural.
	a.scheduleNextProactive(sess, time.Now())
}

// doSpeak sends the bubbles chosen by the model in order.
func (a *Agent) doSpeak(ctx context.Context, ref session.Ref, messages []string) ([]int64, error) {
	if !ref.Valid() {
		return nil, fmt.Errorf("无效的用户")
	}

	ids := make([]int64, 0, len(messages))
	for _, content := range messages {
		if err := a.waitTyping(ctx, content); err != nil {
			return ids, err
		}
		msgID, err := a.bot.SendPrivateMessage(ctx, ref.UserID, content)
		if err != nil {
			zap.L().Error("发言失败", zap.Int64("user_id", ref.UserID), zap.Error(err))
			return ids, err
		}
		ids = append(ids, msgID)
		a.onMessage(&onebot.Message{
			MessageID:      msgID,
			ConversationID: ref.ID(),
			UserID:         a.bot.GetSelfID(),
			Nickname:       a.personaName,
			Content:        content,
			FinalContent:   content,
			Time:           time.Now(),
		})
		zap.L().Info("发言成功", zap.Int64("user_id", ref.UserID), zap.String("content", content))
	}
	return ids, nil
}

func (a *Agent) waitTyping(ctx context.Context, content string) error {
	cfg := config.Get()
	if !cfg.Chat.TypingSimulation {
		return nil
	}
	typingSpeed := cfg.Chat.TypingSpeed
	if typingSpeed <= 0 {
		typingSpeed = 6
	}
	delay := time.Duration(float64(len([]rune(content)))/float64(typingSpeed)*1000) * time.Millisecond
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	if delay < 500*time.Millisecond {
		delay = 500 * time.Millisecond
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// doSendSticker 执行发送表情包，并记录消息
func (a *Agent) doSendSticker(ctx context.Context, ref session.Ref, filePath string, description string) (int64, error) {
	if !ref.Valid() {
		return 0, fmt.Errorf("无效的用户")
	}
	msgID, err := a.bot.SendPrivateImageMessage(ctx, ref.UserID, filePath, true)
	if err != nil {
		zap.L().Error("发送表情包失败", zap.String("ID", ref.ID()), zap.String("path", filePath), zap.Error(err))
		return 0, err
	}

	var content string
	if description != "" {
		content = fmt.Sprintf("[表情包:%s]", description)
	} else {
		content = "[表情包]"
	}

	msg := &onebot.Message{
		MessageID:      msgID,
		ConversationID: ref.ID(),
		UserID:         a.bot.GetSelfID(),
		Nickname:       a.personaName,
		Content:        "",
		FinalContent:   content,
		Time:           time.Now(),
		Images: []onebot.ImageInfo{
			{Summary: content, SubType: 1},
		},
	}
	a.onMessage(msg)
	zap.L().Info("发送表情包成功", zap.String("ID", ref.ID()), zap.String("desc", description))
	return msgID, nil
}

// autoSaveSticker 自动保存表情包（异步执行）
func (a *Agent) autoSaveSticker(ctx context.Context, url string, description string) {
	if url == "" {
		return
	}
	if err := ctx.Err(); err != nil {
		return
	}

	// 获取配置
	cfg := config.Get()
	storagePath := cfg.Sticker.StoragePath
	if storagePath == "" {
		storagePath = "./stickers"
	}
	maxSizeMB := cfg.Sticker.MaxSizeMB
	if maxSizeMB <= 0 {
		maxSizeMB = 2
	}

	// 下载图片
	result, err := utils.DownloadImage(ctx, url, storagePath, maxSizeMB)
	if err != nil {
		zap.L().Debug("下载表情包失败", zap.String("url", url), zap.Error(err))
		return
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(result.FilePath)
		return
	}

	// 如果没有描述，使用默认描述
	if description == "" {
		description = "未描述的表情包"
	}

	// 保存到数据库
	sticker := &memory.Sticker{
		FileName:    result.FileName,
		FileHash:    result.FileHash,
		Description: description,
	}

	isDuplicate, err := a.memory.SaveSticker(sticker)
	if err != nil {
		// 保存失败，删除已下载的文件
		_ = os.Remove(result.FilePath)
		zap.L().Warn("保存表情包失败", zap.Error(err))
		return
	}

	if isDuplicate {
		// 已存在，删除刚下载的文件
		_ = os.Remove(result.FilePath)
		zap.L().Debug("表情包已存在，跳过保存", zap.String("hash", result.FileHash))
		return
	}

	zap.L().Info("自动保存表情包", zap.Uint("id", sticker.ID), zap.String("desc", description))
}
