package tools

import (
	"context"
	"mumu-bot/internal/memory"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// ==================== 审核风格卡片工具 ====================

type GetUncheckedStyleCardsInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"description=返回数量，默认5"`
}

type UncheckedStyleCardItem struct {
	ID            uint   `json:"id"`
	Intent        string `json:"intent"`
	Tone          string `json:"tone"`
	TriggerRule   string `json:"trigger_rule"`
	AvoidRule     string `json:"avoid_rule"`
	Example       string `json:"example"`
	SourceExcerpt string `json:"source_excerpt"`
	EvidenceCount int    `json:"evidence_count"`
}

type GetUncheckedStyleCardsOutput struct {
	Success bool                     `json:"success"`
	Cards   []UncheckedStyleCardItem `json:"cards,omitempty"`
	Message string                   `json:"message,omitempty"`
}

func getUncheckedStyleCardsFunc(ctx context.Context, input *GetUncheckedStyleCardsInput) (*GetUncheckedStyleCardsOutput, error) {
	lc := GetLearningContext(ctx)
	if lc == nil {
		return &GetUncheckedStyleCardsOutput{Success: false, Message: "学习上下文未初始化"}, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 5
	}

	cards, err := lc.MemMgr.ListUncheckedStyleCards(lc.ConversationRef.GroupID, limit)
	if err != nil {
		return &GetUncheckedStyleCardsOutput{Success: false, Message: err.Error()}, nil
	}

	results := make([]UncheckedStyleCardItem, 0, len(cards))
	for _, card := range cards {
		results = append(results, UncheckedStyleCardItem{
			ID:            card.ID,
			Intent:        card.Intent,
			Tone:          card.Tone,
			TriggerRule:   card.TriggerRule,
			AvoidRule:     card.AvoidRule,
			Example:       card.Example,
			SourceExcerpt: card.SourceExcerpt,
			EvidenceCount: card.EvidenceCount,
		})
	}

	return &GetUncheckedStyleCardsOutput{Success: true, Cards: results}, nil
}

func NewGetUncheckedStyleCardsTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"getUncheckedStyleCards",
		"查看待审核的群聊风格卡片，用于判断哪些说话方式足够稳定、可复用且风险可控。",
		getUncheckedStyleCardsFunc,
	)
}

// ==================== 审核风格卡片 ====================

type ReviewStyleCardInput struct {
	IDs     []uint `json:"ids" jsonschema:"description=风格卡片ID列表"`
	Approve bool   `json:"approve" jsonschema:"description=是否通过审核"`
}

type ReviewStyleCardOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func reviewStyleCardFunc(ctx context.Context, input *ReviewStyleCardInput) (*ReviewStyleCardOutput, error) {
	lc := GetLearningContext(ctx)
	if lc == nil {
		return &ReviewStyleCardOutput{Success: false, Message: "学习上下文未初始化"}, nil
	}

	if len(input.IDs) == 0 {
		return &ReviewStyleCardOutput{Success: false, Message: "风格卡片 ID 列表不能为空"}, nil
	}

	err := lc.MemMgr.ReviewStyleCards(input.IDs, input.Approve)
	if err != nil {
		return &ReviewStyleCardOutput{Success: false, Message: err.Error()}, nil
	}

	msg := "已拒绝这些风格卡片"
	if input.Approve {
		msg = "已审核这些风格卡片"
	}
	return &ReviewStyleCardOutput{Success: true, Message: msg}, nil
}

func NewReviewStyleCardTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"reviewStyleCard",
		"批量审核风格卡片。只有足够稳定、可复用且风险可控的卡片才应该通过。",
		reviewStyleCardFunc,
	)
}

// ==================== 保存风格卡片工具 ====================

type SaveStyleCardInput struct {
	Intent        string `json:"intent" jsonschema:"enum=轻松起哄,enum=认同接话,enum=询问推进,enum=安抚缓和,description=风格卡片的意图标签，必须从固定枚举中选择"`
	Tone          string `json:"tone" jsonschema:"enum=直接,enum=轻松,enum=夸张,enum=克制,description=风格卡片的语气标签，必须从固定枚举中选择"`
	TriggerRule   string `json:"trigger_rule" jsonschema:"description=什么时候适合参考这种风格"`
	AvoidRule     string `json:"avoid_rule" jsonschema:"description=什么时候不要参考这种风格"`
	Example       string `json:"example" jsonschema:"description=一条短例句，只做语气味道参考"`
	SourceExcerpt string `json:"source_excerpt,omitempty" jsonschema:"description=原始聊天中的短摘录，用于证明该卡片来源真实"`
}

type SaveStyleCardOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func saveStyleCardFunc(ctx context.Context, input *SaveStyleCardInput) (*SaveStyleCardOutput, error) {
	lc := GetLearningContext(ctx)
	if lc == nil {
		return &SaveStyleCardOutput{Success: false, Message: "学习上下文未初始化"}, nil
	}

	input.Intent = strings.TrimSpace(input.Intent)
	input.Tone = strings.TrimSpace(input.Tone)
	input.TriggerRule = strings.TrimSpace(input.TriggerRule)
	input.AvoidRule = strings.TrimSpace(input.AvoidRule)
	input.Example = strings.TrimSpace(input.Example)
	input.SourceExcerpt = strings.TrimSpace(input.SourceExcerpt)

	if !memory.IsValidStyleIntent(input.Intent) || !memory.IsValidStyleTone(input.Tone) {
		return &SaveStyleCardOutput{Success: false, Message: "失败：风格标签不合法"}, nil
	}
	if input.TriggerRule == "" || input.AvoidRule == "" || input.Example == "" {
		return &SaveStyleCardOutput{Success: false, Message: "失败：缺少必填字段"}, nil
	}

	created, err := lc.MemMgr.SaveStyleCardCandidate(ctx, &memory.StyleCard{
		ConversationID: GetConversationID(ctx),
		GroupID:        lc.ConversationRef.GroupID,
		Intent:         input.Intent,
		Tone:           input.Tone,
		TriggerRule:    input.TriggerRule,
		AvoidRule:      input.AvoidRule,
		Example:        input.Example,
		SourceExcerpt:  input.SourceExcerpt,
		Status:         memory.StyleCardStatusCandidate,
	})
	if err != nil {
		return &SaveStyleCardOutput{Success: false, Message: err.Error()}, nil
	}

	msg := "已更新相近的群聊风格卡片"
	if created {
		msg = "已记住这张新的群聊风格卡片"
	}
	return &SaveStyleCardOutput{Success: true, Message: msg}, nil
}

func NewSaveStyleCardTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"saveStyleCard",
		"保存你从群聊里提炼出的风格卡片。只记录可复用、低风险、能概括群味的说话方式。",
		saveStyleCardFunc,
	)
}

// ==================== 搜索风格卡片工具 ====================

type SearchStyleCardsInput struct {
	Keyword string `json:"keyword" jsonschema:"description=搜索关键词，可以是多个词用空格分隔"`
	Limit   int    `json:"limit,omitempty" jsonschema:"description=返回数量，默认10"`
}

type SearchStyleCardsOutput struct {
	Success bool             `json:"success"`
	Count   int              `json:"count"`
	Cards   []map[string]any `json:"cards,omitempty"`
	Message string           `json:"message,omitempty"`
}

func searchStyleCardsFunc(ctx context.Context, input *SearchStyleCardsInput) (*SearchStyleCardsOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &SearchStyleCardsOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	if strings.TrimSpace(input.Keyword) == "" {
		return &SearchStyleCardsOutput{Success: false, Message: "搜索关键词不能为空"}, nil
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}

	cards, err := tc.MemoryMgr.SearchStyleCards(tc.ConversationRef.GroupID, input.Keyword, limit)
	if err != nil {
		return &SearchStyleCardsOutput{Success: false, Message: err.Error()}, nil
	}

	results := make([]map[string]any, 0, len(cards))
	for _, card := range cards {
		results = append(results, map[string]any{
			"id":                 card.ID,
			"intent":             card.Intent,
			"tone":               card.Tone,
			"trigger_rule":       card.TriggerRule,
			"avoid_rule":         card.AvoidRule,
			"example":            card.Example,
			"evidence_count":     card.EvidenceCount,
			"from_current_group": card.GroupID == tc.ConversationRef.GroupID,
		})
	}

	return &SearchStyleCardsOutput{
		Success: true,
		Count:   len(results),
		Cards:   results,
	}, nil
}

func NewSearchStyleCardsTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"searchStyleCards",
		"搜索已经激活的群聊风格卡片（优先返回来源于本群的卡片）。",
		searchStyleCardsFunc,
	)
}
