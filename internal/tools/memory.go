package tools

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/llm"
	"github.com/kisara71/luma/internal/memory"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// ==================== 保存记忆工具 ====================

// SaveMemoryInput 保存记忆的输入参数
type SaveMemoryInput struct {
	Type string `json:"type" jsonschema:"enum=user_fact,enum=self_experience,enum=conversation,enum=expression,description=user_fact=对方的稳定事实，self_experience=这段关系中的自身经历，conversation=阶段性上下文，expression=这个人特有的词或梗及其语境"`
	// Content 要记住的内容，用自然语言描述
	Content string `json:"content" jsonschema:"description=要记住的内容，用自然语言描述清楚"`
	// Importance 重要性评分，0-1之间
	Importance float64 `json:"importance,omitempty" jsonschema:"minimum=0,maximum=1,description=重要性评分(0-1)，越重要越高"`
}

// SaveMemoryOutput 保存记忆的输出
type SaveMemoryOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// saveMemoryFunc 保存记忆的实际实现
func saveMemoryFunc(ctx context.Context, input *SaveMemoryInput) (*SaveMemoryOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &SaveMemoryOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	if input.Content == "" {
		return &SaveMemoryOutput{Success: false, Message: "内容不能为空"}, nil
	}
	ref := tc.SessionRef()
	if ref.ID() == "" {
		return &SaveMemoryOutput{Success: false, Message: "会话未初始化"}, nil
	}

	// 验证记忆类型
	validTypes := map[string]bool{
		string(memory.MemoryTypeUserFact):       true,
		string(memory.MemoryTypeSelfExperience): true,
		string(memory.MemoryTypeConversation):   true,
		string(memory.MemoryTypeExpression):     true,
	}
	if !validTypes[input.Type] {
		return &SaveMemoryOutput{Success: false, Message: "无效的记忆类型，可选: user_fact, self_experience, conversation, expression"}, nil
	}

	// 向量相似度搜索
	targetType := memory.MemoryType(input.Type)
	similarMems, err := tc.MemoryMgr.SearchSimilarMemoriesByConversation(ctx, input.Content, ref, targetType, 30, 0.85)
	if err == nil && len(similarMems) > 0 {
		// 复用主模型进行合并，避免为低频任务维护第二套模型配置。
		mergeModel, err := llm.NewClient()
		if err == nil {
			var oldContents []string
			for _, m := range similarMems {
				oldContents = append(oldContents, fmt.Sprintf("- %s", m.Content))
			}

			prompt := fmt.Sprintf(`你是一个 Agent 记忆管理员。现在模型试图保存一条新记忆，但发现与现有的记忆高度相似。
请将新记忆与旧记忆合并，生成一条更完整、准确的记忆。

旧记忆：
%s

新记忆：
- %s

要求：
1. 保持客观、简洁。
2. 如果新记忆包含旧记忆没有的细节，请补充进去。
3. 如果新记忆是旧记忆的更新（例如状态变化），请以最新状态为准，但保留重要历史背景。
4. 只输出合并后的记忆内容，不要包含其他废话。`, strings.Join(oldContents, "\n"), input.Content)

			resp, err := mergeModel.Generate(ctx, []*schema.Message{
				schema.UserMessage(prompt),
			})

			if err == nil && resp.Content != "" {
				mergedContent := strings.TrimSpace(resp.Content)

				// 更新最相似的那条记忆（通常是第一条），删除其他的
				firstMem := similarMems[0]
				if err := tc.MemoryMgr.UpdateMemoryContent(ctx, firstMem.ID, mergedContent); err != nil {
					zap.L().Error("合并记忆更新失败", zap.Error(err))
				} else {
					// 删除其他重复的
					for i := 1; i < len(similarMems); i++ {
						_ = tc.MemoryMgr.DeleteMemory(ctx, similarMems[i].ID)
					}

					return &SaveMemoryOutput{Success: true, Message: "已合并并更新相似记忆"}, nil
				}
			}
		} else {
			zap.L().Warn("记忆合并模型初始化失败，跳过记忆合并", zap.Error(err))
		}
	}

	// 如果没有相似记忆或合并失败，直接保存新记忆
	mem := &memory.Memory{
		ConversationID: GetConversationID(ctx),
		Type:           targetType,
		UserID:         ref.UserID,
		Content:        input.Content,
		Importance:     input.Importance,
	}

	if err := tc.MemoryMgr.SaveMemory(ctx, mem); err != nil {
		return &SaveMemoryOutput{Success: false, Message: err.Error()}, nil
	}

	return &SaveMemoryOutput{Success: true, Message: "已记住"}, nil
}

// NewSaveMemoryTool 创建保存记忆工具
func NewSaveMemoryTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"saveMemory",
		`保存当前这段关系中真正重要的信息：
- user_fact：对方稳定的偏好、身份、计划或边界
- self_experience：你和对方共同经历且以后可能会提起的事
- conversation：需要跨越短期窗口保留的阶段性上下文
- expression：这个人特有的词、缩写、梗及其含义和使用语境
注意：普通闲聊不需要保存，只保存真正有价值的信息。`,
		saveMemoryFunc,
	)
}

// ==================== 查询记忆工具 ====================

// QueryMemoryInput 查询记忆的输入参数
type QueryMemoryInput struct {
	// Query 搜索关键词或描述
	Query string `json:"query" jsonschema:"description=搜索关键词或自然语言描述"`
	// Type 限定记忆类型（可选）
	Type string `json:"type,omitempty" jsonschema:"enum=,enum=user_fact,enum=self_experience,enum=conversation,enum=expression,description=筛选记忆类型（空字符串时不筛选）"`
	// Limit 返回结果数量限制，默认10，最大50
	Limit int `json:"limit,omitempty" jsonschema:"description=返回结果数量限制，默认10，最大50"`
}

// QueryMemoryOutput 查询记忆的输出
type QueryMemoryOutput struct {
	Success  bool                     `json:"success"`
	Count    int                      `json:"count"`
	Memories []map[string]interface{} `json:"memories,omitempty"`
	Message  string                   `json:"message,omitempty"`
}

// queryMemoryFunc 查询记忆的实际实现
func queryMemoryFunc(ctx context.Context, input *QueryMemoryInput) (*QueryMemoryOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &QueryMemoryOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	if input.Query == "" {
		return &QueryMemoryOutput{Success: false, Message: "查询内容不能为空"}, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	memories, err := tc.MemoryMgr.QueryMemoryByConversation(ctx, input.Query, tc.SessionRef(), memory.MemoryType(input.Type), limit)
	if err != nil {
		return &QueryMemoryOutput{Success: false, Message: err.Error()}, nil
	}

	results := make([]map[string]interface{}, 0, len(memories))
	for _, m := range memories {
		results = append(results, map[string]interface{}{
			"type":       m.Type,
			"content":    m.Content,
			"importance": m.Importance,
			"created_at": m.CreatedAt.Format("2006-01-02 15:04"),
		})
	}

	return &QueryMemoryOutput{
		Success:  true,
		Count:    len(results),
		Memories: results,
	}, nil
}

// NewQueryMemoryTool 创建查询记忆工具
func NewQueryMemoryTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"queryMemory",
		"搜索与当前聊天对象共同形成的长期记忆。不能搜索其他人的私聊。",
		queryMemoryFunc,
	)
}
