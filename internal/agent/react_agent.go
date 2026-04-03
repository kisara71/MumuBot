package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"mumu-bot/internal/config"
	"mumu-bot/internal/conversation"
	"mumu-bot/internal/jargon"
	"mumu-bot/internal/learning"
	"mumu-bot/internal/llm"
	"mumu-bot/internal/mcp"
	"mumu-bot/internal/memory"
	"mumu-bot/internal/onebot"
	"mumu-bot/internal/persona"
	"mumu-bot/internal/tools"
	"mumu-bot/internal/utils"
	"os"
	"sort"
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
	agentThinkTimeout          = 60 * time.Second
	styleClassificationTimeout = 20 * time.Second
)

// Agent 沐沐智能体
type Agent struct {
	ctx             context.Context
	cancel          context.CancelFunc
	persona         *persona.Persona
	memory          *memory.Manager
	model           model.ToolCallingChatModel
	auxModel        model.ToolCallingChatModel
	vision          *llm.VisionClient // 多模态视觉模型
	bot             *onebot.Client
	react           *react.Agent
	styleClassifier *react.Agent
	tools           []tool.BaseTool
	mcpMgr          *mcp.Manager        // MCP 管理器
	concurrencyMgr  *ConcurrencyManager // 并发管理器

	jargonMgr *jargon.Manager   // 黑话管理器
	learner   *learning.Learner // 后台学习系统

	// 消息缓冲
	buffers   map[string]*utils.RingBuffer[*onebot.Message]
	buffersMu sync.RWMutex

	// 思考聚合窗口
	pendingThinks map[string]*pendingThink
	pendingMu     sync.Mutex

	// 正在处理中的会话和最后处理时间
	processing        map[string]bool
	lastProcessedTime map[string]time.Time
	lastLoopThinkTime map[string]time.Time
	processingMu      sync.RWMutex

	wg sync.WaitGroup
}

type pendingThink struct {
	timer      *time.Timer
	isMention  bool
	fromLoop   bool
	generation uint64
}

func messageConversationRef(msg *onebot.Message) memory.ConversationRef {
	if msg == nil {
		return memory.AllConversationRef()
	}
	if ref, ok := conversation.ParseRefID(msg.ConversationID); ok {
		return ref
	}
	return memory.AllConversationRef()
}

// New 创建 Agent
func New(mem *memory.Manager) (*Agent, error) {
	cfg := config.Get()
	if cfg == nil {
		return nil, fmt.Errorf("配置未加载")
	}

	p := persona.NewPersona(&cfg.Persona)

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
	if err := botClient.Connect(); err != nil {
		return nil, fmt.Errorf("OneBot 连接失败: %w", err)
	}

	rootCtx, cancel := context.WithCancel(context.Background())
	a := &Agent{
		ctx:               rootCtx,
		cancel:            cancel,
		persona:           p,
		memory:            mem,
		model:             chatModel,
		vision:            visionClient,
		bot:               botClient,
		buffers:           make(map[string]*utils.RingBuffer[*onebot.Message]),
		pendingThinks:     make(map[string]*pendingThink),
		processing:        make(map[string]bool),
		lastProcessedTime: make(map[string]time.Time),
		lastLoopThinkTime: make(map[string]time.Time),
	}

	zap.L().Info("人格已加载", zap.String("name", a.persona.GetName()))

	if auxModel, err := llm.NewAuxClient(); err == nil {
		a.auxModel = auxModel
	} else {
		zap.L().Debug("初始化辅助分类模型失败，将回退主模型", zap.Error(err))
	}

	// 初始化并发管理器
	a.concurrencyMgr = NewConcurrencyManager(a.ctx, cfg.Agent.MaxCoroutine, a.think)

	// 初始化黑话管理器
	a.jargonMgr = jargon.New(mem)

	// 初始化后台学习系统
	if cfg.Learning.Enabled {
		learner, err := learning.New(mem, a.jargonMgr)
		if err != nil {
			zap.L().Error("初始化后台学习系统失败", zap.Error(err))
		} else {
			a.learner = learner
		}
	}

	// 初始化 MCP 管理器
	a.mcpMgr = mcp.NewMCPManager()
	if err := a.mcpMgr.LoadFromConfig(a.ctx, "config/mcp.json"); err != nil {
		zap.L().Error("加载 MCP 配置失败", zap.Error(err))
	}

	if err := a.initTools(); err != nil {
		a.bot.Close()
		a.cancel()
		return nil, err
	}
	if err := a.initReact(); err != nil {
		a.bot.Close()
		a.cancel()
		return nil, err
	}
	if err := a.initStyleClassifier(); err != nil {
		a.bot.Close()
		a.cancel()
		return nil, err
	}
	return a, nil
}

func (a *Agent) initTools() error {
	toolBuilders := []func() (tool.BaseTool, error){
		// 记忆相关
		func() (tool.BaseTool, error) { return tools.NewSaveMemoryTool() },
		func() (tool.BaseTool, error) { return tools.NewQueryMemoryTool() },
		// 搜索黑话/表达
		func() (tool.BaseTool, error) { return tools.NewSearchJargonTool() },
		func() (tool.BaseTool, error) { return tools.NewSearchStyleCardsTool() },
		// 用户信息
		func() (tool.BaseTool, error) { return tools.NewUpdateMemberProfileTool() },
		func() (tool.BaseTool, error) { return tools.NewGetMemberInfoTool() },
		func() (tool.BaseTool, error) { return tools.NewGetRecentMessagesTool() },
		// 发言相关
		func() (tool.BaseTool, error) { return tools.NewSpeakTool() },
		func() (tool.BaseTool, error) { return tools.NewStayQuietTool() },
		// 群交互
		func() (tool.BaseTool, error) { return tools.NewGetGroupMemberDetailTool() },
		func() (tool.BaseTool, error) { return tools.NewPokeTool() },
		func() (tool.BaseTool, error) { return tools.NewReactToMessageTool() },
		func() (tool.BaseTool, error) { return tools.NewRecallMessageTool() },
		// 表情包相关
		func() (tool.BaseTool, error) { return tools.NewSearchStickersTool() },
		func() (tool.BaseTool, error) { return tools.NewSendStickerTool() },
		// 群信息
		func() (tool.BaseTool, error) { return tools.NewGetGroupNoticesTool() },
		func() (tool.BaseTool, error) { return tools.NewGetEssenceMessagesTool() },
		func() (tool.BaseTool, error) { return tools.NewGetMessageReactionsTool() },
		func() (tool.BaseTool, error) { return tools.NewGetForwardMessageDetailTool() },
		// 情绪系统
		func() (tool.BaseTool, error) { return tools.NewUpdateMoodTool() },
		// HTTP GET
		func() (tool.BaseTool, error) { return tools.NewHttpRequestTool() },
	}

	for _, build := range toolBuilders {
		t, err := build()
		if err != nil {
			return err
		}
		a.tools = append(a.tools, t)
	}

	// 添加 MCP 工具
	mcpTools := a.mcpMgr.GetTools()
	if len(mcpTools) > 0 {
		a.tools = append(a.tools, mcpTools...)
		zap.L().Info("已加载 MCP 工具", zap.Int("count", len(mcpTools)))
	}

	return nil
}

func (a *Agent) initReact() error {
	cfg := config.Get()
	maxStep := cfg.Agent.MaxStep
	if maxStep <= 0 {
		maxStep = 12 // 默认最大步数
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
		MaxStep:            maxStep,
		ToolReturnDirectly: map[string]struct{}{"stayQuiet": {}},
	})
	if err != nil {
		return err
	}
	a.react = agent
	return nil
}

func (a *Agent) initStyleClassifier() error {
	classifier := a.auxModel
	if classifier == nil {
		classifier = a.model
	}
	if classifier == nil {
		return fmt.Errorf("分类模型未初始化")
	}

	classificationTool, err := tools.NewStyleClassificationTool()
	if err != nil {
		return err
	}

	agent, err := react.NewAgent(a.ctx, &react.AgentConfig{
		ToolCallingModel: classifier,
		ToolsConfig: compose.ToolsNodeConfig{
			Tools:               []tool.BaseTool{classificationTool},
			ExecuteSequentially: true,
		},
		MaxStep:            4,
		ToolReturnDirectly: map[string]struct{}{tools.StyleClassificationToolName: {}},
	})
	if err != nil {
		return err
	}

	a.styleClassifier = agent
	return nil
}

// Start 启动
func (a *Agent) Start() {
	if config.Get().Learning.Enabled && a.learner != nil {
		a.learner.Start(a.ctx)
	}

	// 启动时从数据库加载历史消息到缓冲区
	a.loadBuffersFromDB()

	a.bot.OnMessage(a.onMessage)
	a.wg.Add(1)
	go a.thinkLoop()
	zap.L().Info("Agent 已启动")
}

// loadBuffersFromDB 从数据库加载消息日志到缓冲区
func (a *Agent) loadBuffersFromDB() {
	cfg := config.Get()
	//	群聊
	zap.L().Info("加载群聊信息...")
	for _, gc := range cfg.Groups {
		if !gc.Enabled {
			continue
		}
		logs := a.memory.GetRecentMessages(memory.GroupConversationRef(gc.GroupID), cfg.Agent.MessageBufferSizeGroup, 0)
		if len(logs) == 0 {
			continue
		}
		a.loadMessages(memory.GroupConversationRef(gc.GroupID), cfg.Agent.MessageBufferSizeGroup, logs)
		zap.L().Info(fmt.Sprintf("群聊:%d加载了%d条信息", gc.GroupID, len(logs)))
	}
	zap.L().Info("群聊信息加载完毕")

	//	私聊
	zap.L().Info("加载私聊信息")
	for _, uc := range cfg.Users {
		if !uc.Enabled {
			continue
		}
		logs := a.memory.GetRecentMessages(memory.PrivateConversationRef(uc.UserID), cfg.Agent.MessageBufferSizePrivate, 0)
		if len(logs) == 0 {
			continue
		}
		a.loadMessages(memory.PrivateConversationRef(uc.UserID), cfg.Agent.MessageBufferSizePrivate, logs)
		zap.L().Info(fmt.Sprintf("用户:%d加载了%d条信息", uc.UserID, len(logs)))
	}
	zap.L().Info("私聊信息加载完毕")

}

func (a *Agent) loadMessages(ref memory.ConversationRef, bufSize int, logs []memory.MessageLog) {
	// 获取缓冲区大小
	if bufSize <= 0 {
		bufSize = 15
	}
	// 从数据库获取最近的消息
	if len(logs) == 0 {
		return
	}
	// 初始化缓冲区
	a.buffersMu.Lock()
	buf := utils.NewRingBuffer[*onebot.Message](bufSize)
	a.buffers[ref.ID()] = buf
	// 填充缓冲区
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
			GroupID:        log.GroupID,
			UserID:         log.UserID,
			Nickname:       log.Nickname,
			Content:        log.OriginalContent,
			FinalContent:   log.Content,
			IsMentioned:    log.IsMentioned,
			Time:           log.CreatedAt,
			MessageSource:  onebot.MessageSource(log.MessageSource),
			Forwards:       forwards,
		}
		buf.Push(msg)
	}
	a.buffersMu.Unlock()
}

// Stop 停止
func (a *Agent) Stop() {
	a.cancel()
	a.clearPendingThinks()
	if a.bot != nil {
		_ = a.bot.Close()
	}
	if a.learner != nil {
		a.learner.Stop()
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

func (a *Agent) onMessage(msg *onebot.Message) {
	if err := a.ctx.Err(); err != nil {
		return
	}
	cfg := config.Get()
	switch msg.MessageSource {
	case onebot.MessageSourceGroup:
		if !cfg.IsGroupEnabled(msg.GroupID) {
			return
		}
	case onebot.MessageSourcePrivate:
		if !cfg.IsUserEnabled(msg.UserID) && msg.UserID != a.bot.GetSelfID() {
			return
		}
	default:
		zap.L().Error("invalid message source", zap.Any("msg", msg))
		return
	}

	// 检测是否通过名字或别名提及了沐沐
	isMentioned := msg.IsMentioned || a.persona.IsMentioned(msg.Content)

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

	// 防止注入工具名字
	for _, t := range a.tools {
		info, _ := t.Info(a.ctx)
		parsedContent = strings.ReplaceAll(parsedContent, info.Name, "\"危险指令，已屏蔽\"")
	}

	a.addBuffer(msg)
	_ = a.memory.AddMessage(memory.MessageLog{
		MessageID:       fmt.Sprintf("%d", msg.MessageID),
		ConversationID:  messageConversationRef(msg).ID(),
		GroupID:         msg.GroupID,
		UserID:          msg.UserID,
		Nickname:        msg.Nickname,
		Content:         msg.FinalContent, // 使用解析后的内容
		OriginalContent: msg.Content,
		MessageSource:   string(msg.MessageSource),
		IsMentioned:     isMentioned,
		CreatedAt:       msg.Time,
		Forwards:        forwardsJSON,
	})

	if msg.UserID == a.bot.GetSelfID() {
		return
	}

	if err := a.ctx.Err(); err != nil {
		return
	}
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		a.updateMember(msg)
	}()

	a.scheduleThink(messageConversationRef(msg), isMentioned, false)
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
				a.wg.Add(1)
				go func(url string, stickerDesc string) {
					defer a.wg.Done()
					a.autoSaveSticker(a.ctx, url, stickerDesc)
				}(img.URL, desc)
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
		if name := strings.TrimSpace(a.persona.GetName()); name != "" {
			return fmt.Sprintf("你(%s)", name)
		}
		return "你"
	}

	name := strings.TrimSpace(msg.Nickname)
	if name == "" {
		name = "未知用户"
	}
	if msg.MessageSource == onebot.MessageSourcePrivate {
		return fmt.Sprintf("对方(%s,%d)", name, msg.UserID)
	}
	return fmt.Sprintf("%s(%d)", name, msg.UserID)
}

func (a *Agent) addBuffer(msg *onebot.Message) {
	ref := messageConversationRef(msg)
	if ref.ID() == "" {
		zap.L().Error("消息缺少有效会话信息", zap.Any("msg", msg))
		return
	}
	a.buffersMu.Lock()
	defer a.buffersMu.Unlock()
	buf, ok := a.buffers[ref.ID()]
	if !ok {
		bufSize := config.Get().Agent.MessageBufferSizeGroup
		if ref.IsPrivate() {
			bufSize = config.Get().Agent.MessageBufferSizePrivate
			if bufSize <= 0 {
				bufSize = 50
			}
		} else if bufSize <= 0 {
			bufSize = 15
		}
		buf = utils.NewRingBuffer[*onebot.Message](bufSize)
		a.buffers[ref.ID()] = buf
	}
	buf.Push(msg)
}
func (a *Agent) getBuffer(ref memory.ConversationRef) []*onebot.Message {
	a.buffersMu.RLock()
	buf, ok := a.buffers[ref.ID()]
	a.buffersMu.RUnlock()

	if !ok || buf.IsEmpty() {
		return nil
	}
	return buf.GetAll()
}

func (a *Agent) updateMember(msg *onebot.Message) {
	if err := a.ctx.Err(); err != nil {
		return
	}
	p, err := a.memory.GetOrCreateMemberProfile(msg.UserID, msg.Nickname)
	if err != nil {
		zap.L().Error("获取用户画像失败", zap.Error(err))
		return
	}
	p.MsgCount++
	p.LastSpeak = msg.Time
	p.Nickname = msg.Nickname
	if err := a.memory.UpdateUserProfile(p); err != nil {
		zap.L().Error("更新用户画像失败", zap.Error(err))
	}
}

func (a *Agent) thinkLoop() {
	defer a.wg.Done()
	interval := time.Duration(config.Get().Agent.ThinkInterval) * time.Second
	zap.L().Info("思考循环已启动",
		zap.Duration("interval", interval),
		zap.Int("groups", len(config.Get().Groups)),
		zap.Int("users", len(config.Get().Users)))
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
	for _, gc := range cfg.Groups {
		if !gc.Enabled {
			continue
		}
		ref := memory.GroupConversationRef(gc.GroupID)
		msgs := a.getBuffer(ref)
		if len(msgs) == 0 {
			continue
		}

		lastMsg := msgs[len(msgs)-1]

		// 如果最后一条消息是自己发的，跳过
		if lastMsg.UserID == a.bot.GetSelfID() {
			continue
		}

		// 如果最后一条消息是 @提及，已经在 onMessage 中触发了思考，这里跳过
		if a.persona.IsMentioned(lastMsg.Content) || lastMsg.IsMentioned {
			continue
		}

		if time.Since(lastMsg.Time) > time.Duration(cfg.Agent.ObserveWindow)*time.Second {
			continue
		}
		// 获取当前的发言概率（考虑时段规则）
		speakProb := a.getSpeakProbability(ref)
		if rand.Float64() > speakProb {
			continue
		}
		a.scheduleThink(ref, false, true)
	}

	for _, uc := range cfg.Users {
		if !uc.Enabled {
			zap.L().Debug("私聊 loop 跳过：用户未启用", zap.Int64("user_id", uc.UserID))
			continue
		}
		ref := memory.PrivateConversationRef(uc.UserID)
		msgs := a.getBuffer(ref)
		if len(msgs) == 0 {
			zap.L().Debug("私聊 loop 跳过：没有缓冲消息", zap.String("id", ref.ID()))
			continue
		}

		a.processingMu.RLock()
		lastLoopAt := a.lastLoopThinkTime[ref.ID()]
		a.processingMu.RUnlock()
		if !lastLoopAt.IsZero() && time.Since(lastLoopAt) < time.Duration(cfg.Agent.ThinkInterval)*time.Second {
			zap.L().Debug("私聊 loop 跳过：冷却中",
				zap.String("id", ref.ID()),
				zap.Duration("since_last_loop", time.Since(lastLoopAt)),
				zap.Duration("cooldown", time.Duration(cfg.Agent.ThinkInterval)*time.Second))
			continue
		}

		speakProb := a.getPrivateSpeakProbability(ref)
		roll := rand.Float64()
		if roll > speakProb {
			zap.L().Debug("私聊 loop 未命中发言概率",
				zap.String("id", ref.ID()),
				zap.Float64("prob", speakProb),
				zap.Float64("roll", roll))
			continue
		}
		zap.L().Debug("私聊 loop 命中发言概率",
			zap.String("id", ref.ID()),
			zap.Float64("prob", speakProb),
			zap.Float64("roll", roll))
		a.scheduleThink(ref, false, true)
	}
}

func (a *Agent) scheduleThink(ref memory.ConversationRef, isMention bool, fromLoop bool) {
	debounce := time.Duration(config.Get().Agent.ThinkDebounceMS) * time.Millisecond

	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if pending, ok := a.pendingThinks[ref.ID()]; ok {
		pending.isMention = pending.isMention || isMention
		pending.fromLoop = pending.fromLoop || fromLoop
		pending.generation++
		gen := pending.generation
		pending.timer = time.AfterFunc(debounce, func() {
			a.flushPendingThink(ref, gen)
		})
		return
	}

	if !fromLoop && !isMention && !ref.IsPrivate() {
		return
	}

	pending := &pendingThink{
		isMention:  isMention,
		fromLoop:   fromLoop,
		generation: 1,
	}
	gen := pending.generation
	pending.timer = time.AfterFunc(debounce, func() {
		a.flushPendingThink(ref, gen)
	})
	a.pendingThinks[ref.ID()] = pending
}

func (a *Agent) flushPendingThink(ref memory.ConversationRef, generation uint64) {
	a.pendingMu.Lock()
	pending, ok := a.pendingThinks[ref.ID()]
	if !ok || pending.generation != generation {
		a.pendingMu.Unlock()
		return
	}

	isMention := pending.isMention
	fromLoop := pending.fromLoop
	delete(a.pendingThinks, ref.ID())
	a.pendingMu.Unlock()

	a.concurrencyMgr.Submit(ref, isMention, fromLoop)
}

func (a *Agent) clearPendingThinks() {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()

	for key, pending := range a.pendingThinks {
		if pending.timer != nil {
			pending.timer.Stop()
		}
		delete(a.pendingThinks, key)
	}
}

// getSpeakProbability 获取发言概率（考虑时段规则）
func (a *Agent) getSpeakProbability(ref memory.ConversationRef) float64 {
	cfg := config.Get()
	baseProb := cfg.Chat.TalkFrequency
	// 如果启用了时段规则，则根据当前时间调整概率
	if ref.IsGroup() && cfg.Chat.EnableTimeRules && len(cfg.Chat.TimeRules) > 0 {
		now := time.Now()
		hour := now.Hour()
		minute := now.Minute()
		currentMinutes := hour*60 + minute

		for _, rule := range cfg.Chat.TimeRules {
			// 检查是否适用于当前群（0表示全局）
			if rule.GroupID != 0 && rule.GroupID != ref.GroupID {
				continue
			}
			// 解析时间范围
			var startHour, startMin, endHour, endMin int
			if _, err := fmt.Sscanf(rule.TimeRange, "%d:%d-%d:%d", &startHour, &startMin, &endHour, &endMin); err != nil {
				continue
			}
			startMinutes := startHour*60 + startMin
			endMinutes := endHour*60 + endMin

			// 检查当前时间是否在范围内
			if startMinutes <= endMinutes {
				// 正常时间范围
				if currentMinutes >= startMinutes && currentMinutes < endMinutes {
					baseProb = rule.TalkValue // 使用时段配置的概率覆盖基础概率
					break                     // 找到匹配规则后跳出
				}
			} else {
				// 跨午夜的时间范围
				if currentMinutes >= startMinutes || currentMinutes < endMinutes {
					baseProb = rule.TalkValue
					break
				}
			}
		}
	}

	return a.applyGroupRateLimitProbability(ref, baseProb)
}

func (a *Agent) getPrivateSpeakProbability(ref memory.ConversationRef) float64 {
	// 私聊主动发起比群聊更保守，先按时段给一个基础值，避免深夜打扰。
	baseProb := 0.28
	now := time.Now()
	hour := now.Hour()

	switch {
	case hour >= 0 && hour < 8:
		baseProb = 0.03
	case hour >= 8 && hour < 11:
		baseProb = 0.18
	case hour >= 11 && hour < 18:
		baseProb = 0.24
	case hour >= 18 && hour < 22:
		baseProb = 0.32
	case hour >= 22 && hour < 24:
		baseProb = 0.10
	}

	relationFactor := 0.55
	if profile, err := a.memory.GetMemberProfile(ref.UserID); err == nil && profile != nil {
		// 关系越近越容易主动找对方；respect 作为负增益，表示更克制、不想打扰。
		relationFactor = 0.25 +
			0.45*profile.Intimacy +
			0.30*profile.Familiarity +
			0.15*profile.Trust -
			0.15*profile.Respect
		relationFactor = utils.ClampFloat64(relationFactor, 0.10, 0.95)
	}

	moodFactor := 0.75
	if mood, err := a.memory.GetMoodState(ref.UserID); err == nil && mood != nil {
		// 当下的社交意愿/好奇心会放大主动概率，烦躁会压低它。
		moodFactor = 0.45 +
			0.35*mood.Sociability +
			0.20*mood.Curiosity -
			0.30*mood.Irritation
		moodFactor = utils.ClampFloat64(moodFactor, 0.10, 1.00)
	}

	return a.applyPrivateRateLimitProbability(ref, baseProb*relationFactor*moodFactor)
}

func (a *Agent) applyGroupRateLimitProbability(ref memory.ConversationRef, baseProb float64) float64 {
	limitCfg := config.Get().Chat.RateLimit
	if limitCfg.Enabled && limitCfg.PeriodSec > 0 && limitCfg.MaxMessages > 0 {
		startTime := time.Now().Add(-time.Duration(limitCfg.PeriodSec) * time.Second)
		count, err := a.memory.GetMessageCountByTime(ref, a.bot.GetSelfID(), startTime)
		if err == nil {
			maxMsgs := float64(limitCfg.MaxMessages)
			current := float64(count)

			// 线性衰减系数
			// 当 current=0, decay=1.0; 当 current=max, decay=0.0
			var decay float64
			if current >= maxMsgs {
				decay = 0
			} else {
				decay = (maxMsgs - current) / maxMsgs
			}

			// 应用衰减
			oldProb := baseProb
			baseProb *= decay

			// 保底概率不能把本来更低的基础概率反向抬高。
			minProb := utils.ClampFloat64(math.Min(limitCfg.MinProb, oldProb), 0, 1)
			baseProb = utils.ClampFloat64(baseProb, minProb, 1)

			// 仅在触发衰减时打印日志
			if decay < 1.0 {
				zap.L().Debug("触发防话痨限制",
					zap.String("source", string(ref.Source)),
					zap.String("id", ref.ID()),
					zap.Int64("recent_msgs", count),
					zap.Float64("decay", decay),
					zap.Float64("original_prob", oldProb),
					zap.Float64("new_prob", baseProb))
			}
		}
	}

	return baseProb
}

func (a *Agent) applyPrivateRateLimitProbability(ref memory.ConversationRef, baseProb float64) float64 {
	// 私聊单独限流：只看当前用户会话，且比群聊宽松，避免正常往返被过早压住。
	startTime := time.Now().Add(-20 * time.Minute)
	count, err := a.memory.GetMessageCountByTime(ref, a.bot.GetSelfID(), startTime)
	if err != nil {
		return utils.ClampFloat64(baseProb, 0, 1)
	}

	oldProb := baseProb
	switch {
	case count >= 16:
		baseProb *= 0.10
	case count >= 12:
		baseProb *= 0.30
	case count >= 9:
		baseProb *= 0.55
	case count >= 6:
		baseProb *= 0.80
	default:
		return utils.ClampFloat64(baseProb, 0, 1)
	}

	// 私聊仍保留很低的兜底概率，避免彻底锁死某个会话。
	baseProb = utils.ClampFloat64(baseProb, math.Min(0.02, oldProb), 1)
	zap.L().Debug("触发私聊限流",
		zap.String("source", string(ref.Source)),
		zap.String("id", ref.ID()),
		zap.Int64("recent_msgs", count),
		zap.Float64("original_prob", oldProb),
		zap.Float64("new_prob", baseProb))
	return baseProb
}

// think 提交思考任务
func (a *Agent) think(ref memory.ConversationRef, isMention bool, fromLoop bool) {
	if err := a.ctx.Err(); err != nil {
		return
	}
	if ref.IsGroup() && a.bot.IsSelfMuted(ref.GroupID) {
		return
	}
	// 并发锁：确保同一时间一个群只有一个思考进程
	refID := ref.ID()
	a.processingMu.Lock()
	if a.processing[refID] {
		a.processingMu.Unlock()
		return
	}
	a.processing[refID] = true
	lastProcessedTime := a.lastProcessedTime[refID]
	if fromLoop {
		a.lastLoopThinkTime[refID] = time.Now()
	} else {
		a.lastProcessedTime[refID] = time.Now()
	}
	a.processingMu.Unlock()

	defer func() {
		a.processingMu.Lock()
		a.processing[refID] = false
		a.processingMu.Unlock()
	}()

	ctx := tools.WithToolContext(a.ctx, &tools.ToolContext{
		ConversationRef: ref,
		MemoryMgr:       a.memory,
		Bot:             a.bot,
		SpeakCallback: func(callCtx context.Context, callRef conversation.Ref, content string, replyTo int64, mentions []int64) (int64, error) {
			return a.doSpeak(callCtx, callRef, content, replyTo, mentions)
		},
		SendStickerCallback: func(callCtx context.Context, callRef conversation.Ref, filePath string, description string) (int64, error) {
			return a.doSendSticker(callCtx, callRef, filePath, description)
		},
	})

	// 构建对话上下文
	chatContext := a.buildChatContext(ref, lastProcessedTime)
	if chatContext == "" {
		return
	}

	// 构建动态 prompt 上下文
	promptCtx := &persona.PromptContext{
		Ref: ref,
	}
	if fromLoop {
		promptCtx.LoopInfo = a.buildLoopContext(ref)
	}
	// 主动记忆检索
	if config.Get().Agent.EnableActiveRetrieval {
		promptCtx.RelatedMemories, promptCtx.CrossGroupExperiences = a.buildMemoryContext(ctx, ref)
	}

	// 获取当前目标用户的情绪状态
	if targetUserID := a.resolveMoodTargetUserID(ref); targetUserID > 0 {
		if mood, err := a.memory.GetMoodState(targetUserID); err == nil {
			promptCtx.MoodState = &persona.MoodInfo{
				Valence:     mood.Valence,
				Energy:      mood.Energy,
				Sociability: mood.Sociability,
				Irritation:  mood.Irritation,
				Curiosity:   mood.Curiosity,
			}
		}
	}

	// 注入黑话/梗的解释（AC自动机匹配）
	if a.jargonMgr != nil {
		promptCtx.JargonMatches = a.jargonMgr.Match(chatContext)
	}
	if ref.IsGroup() {
		promptCtx.GroupInfo = a.buildGroupContext(ref)
		promptCtx.StyleHints = a.buildStyleHintContext(ctx, ref)
	} else if ref.IsPrivate() {
		promptCtx.PeerInfo = a.buildPrivatePeerContext(ref)
	}

	// 获取最近在场的人
	recentPeople := ""
	if ref.IsGroup() {
		recentPeople = a.buildRecentPeopleContext(ref)
	}

	// 构建消息
	systemPrompt := a.persona.GetSystemPrompt(ref)

	// 添加专属额外提示词
	extraPrompt := ""
	if ref.IsGroup() {
		if gc := config.Get().GetGroupConfig(ref.GroupID); gc != nil && gc.ExtraPrompt != "" {
			extraPrompt = gc.ExtraPrompt
		}
	} else if ref.IsPrivate() {
		if uc := config.Get().GetUserConfig(ref.UserID); uc != nil && uc.ExtraPrompt != "" {
			extraPrompt = uc.ExtraPrompt
		}
	}

	thinkPrompt := a.persona.GetThinkPrompt(promptCtx, chatContext, extraPrompt, recentPeople)
	if isMention && ref.IsGroup() {
		thinkPrompt += "\n\n注意：有人提到你了，可能在找你说话，你可以看情况回复。"
	}

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
			zap.L().Warn("思考超时", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Duration("timeout", agentThinkTimeout))
		} else if errors.Is(ctxWithTimeout.Err(), context.Canceled) || errors.Is(a.ctx.Err(), context.Canceled) {
			zap.L().Debug("思考已取消", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()))
		} else {
			zap.L().Error("思考失败", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Error(err))
		}
	}

	// 记录 Agent 输出
	if config.Get().Debug.ShowThinking && result != nil && result.Content != "" {
		zap.L().Debug("Agent 输出", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.String("content", result.Content))
	}
}

func (a *Agent) buildLoopContext(ref memory.ConversationRef) string {
	msgs := a.getBuffer(ref)
	if len(msgs) == 0 {
		return ""
	}

	now := time.Now()
	lastOther := time.Time{}
	lastSelf := time.Time{}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg := msgs[i]
		if lastSelf.IsZero() && msg.UserID == a.bot.GetSelfID() {
			lastSelf = msg.Time
		}
		if lastOther.IsZero() && msg.UserID != a.bot.GetSelfID() {
			lastOther = msg.Time
		}
		if !lastSelf.IsZero() && !lastOther.IsZero() {
			break
		}
	}

	lines := []string{
		"- 这是一次定时主动思考，不是对方刚发来新消息",
	}
	if !lastOther.IsZero() {
		lines = append(lines, fmt.Sprintf("- 距离对方上次发言：%s", humanizeElapsed(now.Sub(lastOther))))
	}
	if !lastSelf.IsZero() {
		lines = append(lines, fmt.Sprintf("- 距离你上次发言：%s", humanizeElapsed(now.Sub(lastSelf))))
	}
	return strings.Join(lines, "\n")
}

func humanizeElapsed(d time.Duration) string {
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

func (a *Agent) buildGroupContext(ref memory.ConversationRef) string {
	if a.bot == nil || !ref.IsGroup() {
		return ""
	}

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
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

func (a *Agent) buildPrivatePeerContext(ref memory.ConversationRef) string {
	if !ref.IsPrivate() {
		return ""
	}

	msgs := a.getBuffer(ref)
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

func (a *Agent) resolveMoodTargetUserID(ref memory.ConversationRef) int64 {
	if ref.IsPrivate() {
		return ref.UserID
	}

	msgs := a.getBuffer(ref)
	selfID := a.bot.GetSelfID()
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].UserID != 0 && msgs[i].UserID != selfID {
			return msgs[i].UserID
		}
	}
	return 0
}

func (a *Agent) buildMemoryContext(ctx context.Context, ref memory.ConversationRef) ([]memory.Memory, []memory.Memory) {
	msgs := a.getBuffer(ref)
	if len(msgs) == 0 {
		return nil, nil
	}

	query := collectTextContext(msgs)
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

	cross, err := a.memory.SearchSimilarMemoriesByConversation(ctx, query, conversation.AllConversationRef(), memory.MemoryTypeSelfExperience, 4, threshold)
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

func collectTextContext(msgs []*onebot.Message) string {
	msgs = sortedMessages(msgs)
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

func sortedMessages(msgs []*onebot.Message) []*onebot.Message {
	if len(msgs) <= 1 {
		return msgs
	}

	cloned := append([]*onebot.Message(nil), msgs...)
	sort.SliceStable(cloned, func(i, j int) bool {
		if cloned[i] == nil || cloned[j] == nil {
			return i < j
		}
		if cloned[i].MessageID > 0 && cloned[j].MessageID > 0 && cloned[i].MessageID != cloned[j].MessageID {
			return cloned[i].MessageID < cloned[j].MessageID
		}
		if !cloned[i].Time.Equal(cloned[j].Time) {
			return cloned[i].Time.Before(cloned[j].Time)
		}
		return i < j
	})
	return cloned
}

func (a *Agent) buildStyleHintContext(ctx context.Context, ref memory.ConversationRef) []string {
	if !ref.IsGroup() {
		return nil
	}
	classification, err := a.classifyStyleContext(ctx, ref)
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

	hints := buildStyleHints(classification.Intent, cards)
	usedIDs := make([]uint, 0, len(cards))
	for _, card := range cards {
		usedIDs = append(usedIDs, card.ID)
	}
	if err := a.memory.IncrementStyleCardUsage(usedIDs); err != nil {
		zap.L().Debug("更新风格卡片使用计数失败", zap.Int64("group_id", ref.GroupID), zap.Error(err))
	}

	return hints
}

func (a *Agent) classifyStyleContext(ctx context.Context, ref memory.ConversationRef) (*tools.StyleClassification, error) {
	if !ref.IsGroup() {
		return nil, fmt.Errorf("私聊不需要群风格分类")
	}
	contextText := collectTextContext(a.getBuffer(ref))
	if contextText == "" {
		return nil, fmt.Errorf("没有可分类的文字消息")
	}
	if a.styleClassifier == nil {
		return nil, fmt.Errorf("分类 Agent 未初始化")
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

func buildStyleHints(intent string, cards []memory.StyleCard) []string {
	hints := make([]string, 0, len(cards)+1)
	hints = append(hints, "当前推荐发言方向："+intent)
	for _, card := range cards {
		hints = append(hints, formatStyleHint(card))
	}
	return hints
}

func formatStyleHint(card memory.StyleCard) string {
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

// buildChatContext 构建聊天上下文
func (a *Agent) buildChatContext(ref memory.ConversationRef, lastProcessedTime time.Time) string {
	msgs := sortedMessages(a.getBuffer(ref))
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

// buildRecentPeopleContext 获取最近在场的人
func (a *Agent) buildRecentPeopleContext(ref memory.ConversationRef) string {
	msgs := a.getBuffer(ref)
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
		interests := strings.TrimSpace(profile.Interests)
		if interests != "" {
			var items []string
			if err := sonic.UnmarshalString(interests, &items); err == nil && len(items) > 0 {
				interests = strings.Join(items, "、")
			}
		}
		if interests != "" {
			details = append(details, "兴趣: "+interests)
		}

		lines = append(lines, fmt.Sprintf("- %s：%s。", displayName, strings.Join(details, "，")))
	}

	return strings.Join(lines, "\n")
}

// doSpeak 执行发言，返回消息ID
func (a *Agent) doSpeak(ctx context.Context, ref memory.ConversationRef, content string, replyTo int64, mentions []int64) (int64, error) {
	// 模拟打字延迟
	cfg := config.Get()
	if cfg.Chat.TypingSimulation {
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
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return 0, ctx.Err()
		case <-timer.C:
		}
	}

	var (
		msgID int64
		err   error
	)
	switch {
	case ref.IsGroup():
		msgID, err = a.bot.SendGroupMessage(ctx, ref.GroupID, content, replyTo, mentions)
	case ref.IsPrivate():
		msgID, err = a.bot.SendPrivateMessage(ctx, ref.UserID, content)
	default:
		return 0, fmt.Errorf("无效的会话类型")
	}
	if err != nil {
		zap.L().Error("发言失败", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.Error(err))
		return 0, err
	}

	msg := &onebot.Message{
		MessageID:      msgID,
		ConversationID: ref.ID(),
		GroupID:        ref.GroupID,
		UserID:         a.bot.GetSelfID(),
		Nickname:       a.persona.GetName(),
		Content:        content,
		FinalContent:   content,
		Time:           time.Now(),
		MessageSource:  ref.Source,
	}
	a.onMessage(msg)
	zap.L().Info("发言成功", zap.String("source", string(ref.Source)), zap.String("id", ref.ID()), zap.String("content", content))
	return msgID, nil
}

// doSendSticker 执行发送表情包，并记录消息
func (a *Agent) doSendSticker(ctx context.Context, ref memory.ConversationRef, filePath string, description string) (int64, error) {
	var (
		msgID int64
		err   error
	)
	switch {
	case ref.IsGroup():
		msgID, err = a.bot.SendGroupImageMessage(ctx, ref.GroupID, filePath, true)
	case ref.IsPrivate():
		msgID, err = a.bot.SendPrivateImageMessage(ctx, ref.UserID, filePath, true)
	default:
		return 0, fmt.Errorf("无效的会话类型")
	}
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
		GroupID:        ref.GroupID,
		UserID:         a.bot.GetSelfID(),
		Nickname:       a.persona.GetName(),
		Content:        "",
		FinalContent:   content,
		Time:           time.Now(),
		MessageSource:  ref.Source,
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
