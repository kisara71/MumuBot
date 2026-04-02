package tools

import (
	"context"
	"fmt"
	"mumu-bot/internal/memory"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// ==================== 保存黑话工具 ====================

// SaveJargonInput 保存黑话的输入参数
type SaveJargonInput struct {
	// Content 黑话/术语/梗的内容
	Content string `json:"content" jsonschema:"description=黑话、术语或梗的原文"`
	// Meaning 含义解释
	Meaning string `json:"meaning" jsonschema:"description=这个黑话/术语的含义或解释"`
	// Context 使用场景或上下文
	Context string `json:"context,omitempty" jsonschema:"description=在什么情况下使用，或者来源背景"`
}

// SaveJargonOutput 保存黑话的输出
type SaveJargonOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// saveJargonFunc 保存黑话的实际实现
func saveJargonFunc(ctx context.Context, input *SaveJargonInput) (*SaveJargonOutput, error) {
	lc := GetLearningContext(ctx)
	if lc == nil {
		return &SaveJargonOutput{Success: false, Message: "学习上下文未初始化"}, nil
	}

	if input.Content == "" || input.Meaning == "" {
		return &SaveJargonOutput{Success: false, Message: "失败：黑话内容和含义不能为空"}, nil
	}

	// 先查找是否存在
	existingJargons, err := lc.MemMgr.SearchJargonsByConversation(lc.ConversationRef, input.Content, 1)
	var existing *memory.Jargon
	if err == nil && len(existingJargons) > 0 {
		// 精确匹配检查
		if existingJargons[0].Content == input.Content {
			existing = &existingJargons[0]
		}
	}

	if existing != nil {
		// 更新现有黑话
		existing.Meaning = input.Meaning
		if input.Context != "" {
			existing.Context = input.Context
		}
		// 重新置为未验证，需要人工再次审核
		existing.Checked = false
		existing.Rejected = false

		if err := lc.MemMgr.SaveJargon(existing); err != nil {
			return &SaveJargonOutput{Success: false, Message: err.Error()}, nil
		}

		msg := fmt.Sprintf("已更新黑话 '%s' 的含义", input.Content)
		return &SaveJargonOutput{Success: true, Message: msg}, nil
	}

	// 新建黑话
	groupID := lc.GroupID
	userID := int64(0)
	if lc.ConversationRef.IsPrivate() {
		groupID = 0
		userID = lc.ConversationRef.UserID
	}
	jargon := &memory.Jargon{
		GroupID:  groupID,
		UserID:   userID,
		Content:  input.Content,
		Meaning:  input.Meaning,
		Context:  input.Context,
		Checked:  false, // 学习模式下默认为未验证
		Rejected: false,
	}

	if err := lc.MemMgr.SaveJargon(jargon); err != nil {
		return &SaveJargonOutput{Success: false, Message: err.Error()}, nil
	}

	// 实时更新 AC 自动机
	if lc.JargonMgr != nil {
		lc.JargonMgr.AddJargon(input.Content, input.Meaning)
	}

	return &SaveJargonOutput{Success: true, Message: "已记住这个黑话"}, nil
}

// NewSaveJargonTool 创建保存黑话工具
func NewSaveJargonTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"saveJargon",
		`保存群里的黑话、术语或梗。可重复保存，会覆盖已有的记录。`,
		saveJargonFunc,
	)
}

// ==================== 搜索黑话工具 ====================

// SearchJargonInput 搜索黑话的输入参数
type SearchJargonInput struct {
	// Keyword 搜索关键词
	Keyword string `json:"keyword" jsonschema:"description=搜索关键词，可以是多个词用空格分隔"`
	// Limit 返回结果数量限制，默认10
	Limit int `json:"limit,omitempty" jsonschema:"description=返回结果数量限制，默认10"`
}

// SearchJargonOutput 搜索黑话的输出
type SearchJargonOutput struct {
	Success bool             `json:"success"`
	Count   int              `json:"count"`
	Jargons []map[string]any `json:"jargons,omitempty"`
	Message string           `json:"message,omitempty"`
}

// searchJargonFunc 搜索黑话的实际实现
func searchJargonFunc(ctx context.Context, input *SearchJargonInput) (*SearchJargonOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &SearchJargonOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	if input.Keyword == "" {
		return &SearchJargonOutput{Success: false, Message: "搜索关键词不能为空"}, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}

	jargons, err := tc.MemoryMgr.SearchJargonsByConversation(tc.ConversationRef, input.Keyword, limit)
	if err != nil {
		return &SearchJargonOutput{Success: false, Message: err.Error()}, nil
	}

	results := make([]map[string]any, 0, len(jargons))
	for _, j := range jargons {
		results = append(results, map[string]any{
			"id":                 j.ID,
			"content":            j.Content,
			"meaning":            j.Meaning,
			"context":            j.Context,
			"checked":            j.Checked,
			"from_current_group": j.GroupID == tc.GroupID,
		})
	}

	return &SearchJargonOutput{
		Success: true,
		Count:   len(results),
		Jargons: results,
	}, nil
}

// NewSearchJargonTool 创建搜索黑话工具
func NewSearchJargonTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"searchJargon",
		`搜索已保存的黑话、术语或梗（优先搜索来源于本群的）。`,
		searchJargonFunc,
	)
}

// ==================== 获取待审核黑话工具 ====================

type GetUncheckedJargonsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"description=返回数量，默认5"`
}

type UncheckedJargonItem struct {
	ID      uint   `json:"id"`
	Content string `json:"content"`
	Meaning string `json:"meaning"`
	Context string `json:"context"`
}

type GetUncheckedJargonsOutput struct {
	Success bool                  `json:"success"`
	Jargons []UncheckedJargonItem `json:"jargons,omitempty"`
	Message string                `json:"message,omitempty"`
}

func getUncheckedJargonsFunc(ctx context.Context, input *GetUncheckedJargonsInput) (*GetUncheckedJargonsOutput, error) {
	lc := GetLearningContext(ctx)
	if lc == nil {
		return &GetUncheckedJargonsOutput{Success: false, Message: "学习上下文未初始化"}, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}

	jargons, err := lc.MemMgr.GetUncheckedJargonsByConversation(lc.ConversationRef, limit)
	if err != nil {
		return &GetUncheckedJargonsOutput{Success: false, Message: err.Error()}, nil
	}

	results := make([]UncheckedJargonItem, 0, len(jargons))
	for _, j := range jargons {
		results = append(results, UncheckedJargonItem{
			ID:      j.ID,
			Content: j.Content,
			Meaning: j.Meaning,
			Context: j.Context,
		})
	}

	return &GetUncheckedJargonsOutput{Success: true, Jargons: results}, nil
}

func NewGetUncheckedJargonsTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"getUncheckedJargons",
		"查看待审核的黑话/术语。你可以检查这些黑话的含义是否准确。",
		getUncheckedJargonsFunc,
	)
}

// ==================== 审核黑话工具 ====================

type ReviewJargonInput struct {
	IDs     []uint `json:"ids" jsonschema:"description=黑话ID列表"`
	Approve bool   `json:"approve" jsonschema:"description=是否通过审核"`
}

type ReviewJargonOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func reviewJargonFunc(ctx context.Context, input *ReviewJargonInput) (*ReviewJargonOutput, error) {
	lc := GetLearningContext(ctx)
	if lc == nil {
		return &ReviewJargonOutput{Success: false, Message: "学习上下文未初始化"}, nil
	}

	if len(input.IDs) == 0 {
		return &ReviewJargonOutput{Success: false, Message: "黑话 ID 列表不能为空"}, nil
	}

	err := lc.MemMgr.BatchReviewJargon(input.IDs, input.Approve)
	if err != nil {
		return &ReviewJargonOutput{Success: false, Message: err.Error()}, nil
	}

	msg := "已拒绝这些黑话"
	if input.Approve {
		msg = "已验证这些黑话"
	}
	return &ReviewJargonOutput{Success: true, Message: msg}, nil
}

func NewReviewJargonTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"reviewJargon",
		"批量审核黑话/术语。如果含义正确，可以通过验证；如果有误，可以拒绝。",
		reviewJargonFunc,
	)
}
