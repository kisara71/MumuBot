package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

type SpeakInput struct {
	Messages []string `json:"messages" jsonschema:"minItems=1,maxItems=4,description=按真实聊天节奏发送的气泡列表；由你决定是一条完整消息还是几条连续短消息"`
}

type SpeakOutput struct {
	Success    bool    `json:"success"`
	MessageIDs []int64 `json:"message_ids,omitempty"`
	Message    string  `json:"message,omitempty"`
}

func speakFunc(ctx context.Context, input *SpeakInput) (*SpeakOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil || tc.SpeakCallback == nil {
		return &SpeakOutput{Success: false, Message: "发言上下文未初始化"}, nil
	}
	if len(input.Messages) == 0 || len(input.Messages) > 4 {
		return &SpeakOutput{Success: false, Message: "消息气泡数量必须在 1 到 4 之间"}, nil
	}
	messages := make([]string, 0, len(input.Messages))
	for _, raw := range input.Messages {
		content := strings.TrimSpace(raw)
		if content == "" {
			return &SpeakOutput{Success: false, Message: "消息气泡不能为空"}, nil
		}
		if len([]rune(content)) > 500 {
			return &SpeakOutput{Success: false, Message: "单条消息太长"}, nil
		}
		messages = append(messages, content)
	}
	if !tc.ReserveTerminalAction() {
		return &SpeakOutput{Success: false, Message: "本轮已经完成"}, nil
	}
	msgIDs, err := tc.SpeakCallback(ctx, tc.SessionRef(), messages)
	if err != nil {
		return &SpeakOutput{Success: false, Message: err.Error()}, nil
	}
	return &SpeakOutput{Success: true, MessageIDs: msgIDs, Message: fmt.Sprintf("已发送 %d 条消息", len(msgIDs))}, nil
}

func NewSpeakTool() (tool.InvokableTool, error) {
	return utils.InferTool("speak", `发送文字消息。你可以根据真实聊天节奏选择一条完整消息，或拆成少量连续短气泡。`, speakFunc)
}

type StayQuietInput struct {
	Reason string `json:"reason,omitempty" jsonschema:"description=不回复的简短原因，仅用于日志"`
}

type StayQuietOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

func stayQuietFunc(ctx context.Context, _ *StayQuietInput) (*StayQuietOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil || !tc.ReserveTerminalAction() {
		return &StayQuietOutput{Success: false, Message: "本轮已经完成"}, nil
	}
	return &StayQuietOutput{Success: true, Message: "保持沉默"}, nil
}

func NewStayQuietTool() (tool.InvokableTool, error) {
	return utils.InferTool("stayQuiet", "现在不发送消息。", stayQuietFunc)
}
