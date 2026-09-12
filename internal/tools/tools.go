package tools

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/memory"
	"github.com/kisara71/luma/internal/onebot"
	"github.com/kisara71/luma/internal/session"
	"strings"
	"sync"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"go.uber.org/zap"
)

type SpeakCallback func(context.Context, session.Ref, []string) ([]int64, error)
type SendStickerCallback func(context.Context, session.Ref, string, string) (int64, error)

type ToolContext struct {
	Session             *session.Session[*onebot.Message]
	MemoryMgr           *memory.Manager
	Bot                 *onebot.Client
	SpeakCallback       SpeakCallback
	SendStickerCallback SendStickerCallback

	seenMu        sync.Mutex
	seenToolCalls map[string]struct{}
	terminalTaken bool
}

type ctxKey string

const (
	toolContextKey ctxKey = "tool_context"
	toolLogMaxLen         = 1000
)

func WithToolContext(ctx context.Context, tc *ToolContext) context.Context {
	return context.WithValue(ctx, toolContextKey, tc)
}

func GetToolContext(ctx context.Context) *ToolContext {
	tc, _ := ctx.Value(toolContextKey).(*ToolContext)
	return tc
}

func (tc *ToolContext) SessionRef() session.Ref {
	if tc == nil || tc.Session == nil {
		return session.Ref{}
	}
	return tc.Session.Ref
}

func (tc *ToolContext) MarkToolCallSeen(name, arguments string) bool {
	key := name + "-" + arguments
	tc.seenMu.Lock()
	defer tc.seenMu.Unlock()
	if tc.seenToolCalls == nil {
		tc.seenToolCalls = make(map[string]struct{}, 8)
	}
	if _, exists := tc.seenToolCalls[key]; exists {
		return true
	}
	tc.seenToolCalls[key] = struct{}{}
	return false
}

func (tc *ToolContext) ReserveTerminalAction() bool {
	if tc == nil {
		return false
	}
	tc.seenMu.Lock()
	defer tc.seenMu.Unlock()
	if tc.terminalTaken {
		return false
	}
	tc.terminalTaken = true
	return true
}

func (tc *ToolContext) TerminalActionTaken() bool {
	if tc == nil {
		return false
	}
	tc.seenMu.Lock()
	defer tc.seenMu.Unlock()
	return tc.terminalTaken
}

func GetConversationID(ctx context.Context) string {
	if tc := GetToolContext(ctx); tc != nil && tc.Session != nil {
		return tc.Session.Ref.ID()
	}
	return ""
}

func truncateToolLogString(raw string, max int) string {
	trimmed := strings.TrimSpace(raw)
	runes := []rune(trimmed)
	if max <= 0 {
		return ""
	}
	if len(runes) <= max {
		return trimmed
	}
	return string(runes[:max]) + "...(truncated)"
}

func LogToolCall(name, inputJSON, outputJSON string, err error) {
	cfg := config.Get()
	if cfg == nil || !cfg.Debug.ShowToolCalls {
		return
	}
	fields := []zap.Field{
		zap.String("tool", name),
		zap.String("input", truncateToolLogString(inputJSON, toolLogMaxLen)),
		zap.String("output", truncateToolLogString(outputJSON, toolLogMaxLen)),
	}
	if err != nil {
		fields = append(fields, zap.Error(err))
	}
	zap.L().Debug("工具调用", fields...)
}

type GetRecentMessagesInput struct {
	Limit  int `json:"limit,omitempty" jsonschema:"description=返回消息条数，默认30，最大80"`
	Offset int `json:"offset,omitempty" jsonschema:"description=跳过最近多少条消息"`
}

type GetRecentMessagesOutput struct {
	Success  bool                     `json:"success"`
	Messages []map[string]interface{} `json:"messages,omitempty"`
	Message  string                   `json:"message,omitempty"`
}

func getRecentMessagesFunc(ctx context.Context, input *GetRecentMessagesInput) (*GetRecentMessagesOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil || tc.MemoryMgr == nil {
		return &GetRecentMessagesOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 80 {
		limit = 80
	}
	messages := tc.MemoryMgr.GetRecentMessages(tc.SessionRef(), limit, input.Offset)
	result := make([]map[string]interface{}, 0, len(messages))
	for _, msg := range messages {
		result = append(result, map[string]interface{}{
			"speaker": msg.Nickname, "content": msg.Content, "time": msg.CreatedAt.Format("2006-01-02 15:04"),
		})
	}
	return &GetRecentMessagesOutput{Success: true, Messages: result}, nil
}

func NewGetRecentMessagesTool() (tool.InvokableTool, error) {
	return utils.InferTool("getRecentMessages", "读取当前这个人的更早私聊记录。只在当前上下文不够时使用。", getRecentMessagesFunc)
}

type GetForwardMessageDetailInput struct {
	MessageID int64 `json:"message_id" jsonschema:"description=包含合并转发内容的消息ID"`
}

type GetForwardMessageDetailOutput struct {
	Success  bool                    `json:"success"`
	Message  string                  `json:"message,omitempty"`
	Forwards []onebot.ForwardMessage `json:"forwards,omitempty"`
}

func getForwardMessageDetailFunc(ctx context.Context, input *GetForwardMessageDetailInput) (*GetForwardMessageDetailOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil || tc.MemoryMgr == nil {
		return &GetForwardMessageDetailOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}
	log, err := tc.MemoryMgr.GetMessageLogByID(fmt.Sprintf("%d", input.MessageID))
	if err != nil || log.ConversationID != tc.SessionRef().ID() {
		return &GetForwardMessageDetailOutput{Success: false, Message: "当前私聊中没有这条消息"}, nil
	}
	if log.Forwards == "" {
		return &GetForwardMessageDetailOutput{Success: false, Message: "这条消息不包含合并转发"}, nil
	}
	var forwards []onebot.ForwardMessage
	if err := sonic.UnmarshalString(log.Forwards, &forwards); err != nil {
		return &GetForwardMessageDetailOutput{Success: false, Message: "解析合并转发失败"}, nil
	}
	return &GetForwardMessageDetailOutput{Success: true, Forwards: forwards}, nil
}

func NewGetForwardMessageDetailTool() (tool.InvokableTool, error) {
	return utils.InferTool("getForwardMessageDetail", "查看当前私聊中某条合并转发消息的内容。", getForwardMessageDetailFunc)
}
