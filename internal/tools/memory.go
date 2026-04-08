package tools

import (
	"context"
	"fmt"
	"mumu-bot/internal/llm"
	"mumu-bot/internal/memory"
	"mumu-bot/internal/session"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"go.uber.org/zap"
)

// ==================== 保存记忆工具 ====================

// SaveMemoryInput 保存记忆的输入参数
type SaveMemoryInput struct {
	// Type 记忆类型：group_fact(群事实)、user_fact(用户事实)、self_experience(自身经历)、conversation(对话)
	Type string `json:"type" jsonschema:"enum=group_fact,enum=user_fact,enum=self_experience,enum=conversation,description=记忆类型：group_fact=群相关的长期事实、user_fact=某个用户的稳定事实（私聊优先）、self_experience=你自己的经历和感受、conversation=对话中的重要信息"`
	// Content 要记住的内容，用自然语言描述
	Content string `json:"content" jsonschema:"description=要记住的内容，用自然语言描述清楚"`
	// Importance 重要性评分，0-1之间
	Importance float64 `json:"importance,omitempty" jsonschema:"minimum=0,maximum=1,description=重要性评分(0-1)，越重要越高"`
	// RelatedUserID 相关的用户ID（可选）
	RelatedUserID int64 `json:"related_user_id,omitempty" jsonschema:"description=如果这条记忆与某个用户相关，填写其QQ号，否则填0"`
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
		string(memory.MemoryTypeGroupFact):      true,
		string(memory.MemoryTypeUserFact):       true,
		string(memory.MemoryTypeSelfExperience): true,
		string(memory.MemoryTypeConversation):   true,
	}
	if !validTypes[input.Type] {
		return &SaveMemoryOutput{Success: false, Message: "无效的记忆类型，可选: group_fact, user_fact, self_experience, conversation"}, nil
	}

	// 向量相似度搜索
	targetType := memory.MemoryType(input.Type)
	if targetType == memory.MemoryTypeUserFact && !ref.IsPrivate() {
		return &SaveMemoryOutput{Success: false, Message: "user_fact 仅允许在私聊中保存"}, nil
	}
	similarMems, err := tc.MemoryMgr.SearchSimilarMemoriesByConversation(ctx, input.Content, ref, targetType, 30, 0.85)
	if err == nil && len(similarMems) > 0 {
		// 调用辅助模型进行合并
		auxModel, err := llm.NewAuxClient()
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

			resp, err := auxModel.Generate(ctx, []*schema.Message{
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
			zap.L().Warn("辅助模型初始化失败，跳过记忆合并", zap.Error(err))
		}
	}

	// 如果没有相似记忆或合并失败，直接保存新记忆
	groupID := ref.GroupID
	userID := input.RelatedUserID
	if ref.IsPrivate() {
		groupID = 0
		if userID == 0 {
			userID = ref.UserID
		}
	}
	mem := &memory.Memory{
		ConversationID: GetConversationID(ctx),
		Type:           targetType,
		GroupID:        groupID,
		UserID:         userID,
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
		`保存重要信息到长期记忆。当你发现以下情况时应该使用：
- group_fact：群规、群特色、群里的重要事件、某个话题的结论等
- user_fact：仅在私聊中使用，保存对方相对稳定的信息，比如偏好、状态、计划
- self_experience：你参与的有趣对话、被@的经历、你的主观感受和想法
- conversation：群友说的重要事情、有价值的信息、值得记住的对话内容
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
	Type string `json:"type,omitempty" jsonschema:"enum=,enum=group_fact,enum=user_fact,enum=self_experience,enum=conversation,description=筛选记忆类型（空字符串时不筛选）"`
	// Scoped 是否只搜索当前会话的记忆
	Scoped bool `json:"scoped,omitempty" jsonschema:"description=是否只搜索当前会话的记忆，默认false"`
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

	ref := session.AllConversationRef()
	if input.Scoped {
		ref = tc.SessionRef()
	}

	memories, err := tc.MemoryMgr.QueryMemoryByConversation(ctx, input.Query, ref, memory.MemoryType(input.Type), limit)
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
		`搜索你的记忆，找到相关的信息。可以查询关于某个话题、某个人、或者某次经历的记忆。

【scoped 参数使用指南】
- scoped=false（默认）：搜索所有会话的记忆，适合查找自身经历、过往事件等
- scoped=true：只搜索当前会话的记忆
`,
		queryMemoryFunc,
	)
}
