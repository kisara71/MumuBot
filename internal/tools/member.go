package tools

import (
	"context"

	mutils "mumu-bot/internal/utils"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"go.uber.org/zap"
)

// ==================== 更新成员画像工具 ====================

// mergeAndDeduplicateStrings 合并两个字符串切片并去重
func mergeAndDeduplicateStrings(existing []string, newItems []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)

	// 先添加已有的
	for _, item := range existing {
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}

	// 再添加新的
	for _, item := range newItems {
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}

	return result
}

// UpdateUserProfileInput 更新成员画像的输入参数
type UpdateUserProfileInput struct {
	// UserID 群友的QQ号
	UserID int64 `json:"user_id" jsonschema:"description=群友/私聊对象的QQ号"`
	// SpeakStyle 说话风格描述
	SpeakStyle string `json:"speak_style,omitempty" jsonschema:"description=说话风格描述（覆盖之前的描述）"`
	// Interests 兴趣爱好列表
	Interests []string `json:"interests,omitempty" jsonschema:"description=兴趣爱好列表（只传入新增的项）"`
	// CommonWords 常用词汇或口头禅
	CommonWords []string `json:"common_words,omitempty" jsonschema:"description=常用词汇或口头禅（只传入新增的项）"`
	// IntimacyDelta 亲密度变化值 -0.3 到 0.3
	IntimacyDelta float64 `json:"intimacy_delta,omitempty" jsonschema:"minimum=-0.3,maximum=0.3,description=亲密度变化值(-0.3到0.3)，正数表示增加亲密度，负数表示降低亲密度"`
}

// UpdateMemberProfileOutput 更新成员画像的输出
type UpdateMemberProfileOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// updateMemberProfileFunc 更新成员画像的实际实现
func updateMemberProfileFunc(ctx context.Context, input *UpdateUserProfileInput) (*UpdateMemberProfileOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &UpdateMemberProfileOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	if input.UserID == 0 {
		return &UpdateMemberProfileOutput{Success: false, Message: "用户 ID 不能为空"}, nil
	}

	profile, err := tc.MemoryMgr.GetMemberProfile(input.UserID)
	if err != nil {
		return &UpdateMemberProfileOutput{Success: false, Message: err.Error()}, nil
	}

	if input.SpeakStyle != "" {
		profile.SpeakStyle = input.SpeakStyle
	}
	if len(input.Interests) > 0 {
		// 解析已有的兴趣爱好
		var existingInterests []string
		if profile.Interests != "" {
			if err := sonic.UnmarshalString(profile.Interests, &existingInterests); err != nil {
				existingInterests = []string{}
			}
		}
		// 合并并去重
		mergedInterests := mergeAndDeduplicateStrings(existingInterests, input.Interests)
		b, _ := sonic.MarshalString(mergedInterests)
		profile.Interests = b
	}
	if len(input.CommonWords) > 0 {
		// 解析已有的常用词汇
		var existingCommonWords []string
		if profile.CommonWords != "" {
			if err := sonic.UnmarshalString(profile.CommonWords, &existingCommonWords); err != nil {
				existingCommonWords = []string{}
			}
		}
		// 合并并去重
		mergedCommonWords := mergeAndDeduplicateStrings(existingCommonWords, input.CommonWords)
		b, _ := sonic.MarshalString(mergedCommonWords)
		profile.CommonWords = b
	}

	delta := input.IntimacyDelta
	profile.Intimacy = mutils.ClampFloat64(profile.Intimacy+delta, 0, 1)

	if err := tc.MemoryMgr.UpdateUserProfile(profile); err != nil {
		return &UpdateMemberProfileOutput{Success: false, Message: err.Error()}, nil
	}

	return &UpdateMemberProfileOutput{Success: true, Message: "已更新对该用户的了解"}, nil
}

// NewUpdateMemberProfileTool 创建更新成员画像工具
func NewUpdateMemberProfileTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"updateMemberProfile",
		"更新你对某个群友/私聊对象的了解。当你发现群友/私聊对象的新特点、说话风格、兴趣爱好时使用。也可以根据互动情况调整亲密度。",
		updateMemberProfileFunc,
	)
}

// ==================== 获取成员信息工具 ====================

// GetUserInfoInput 获取成员信息的输入参数
type GetUserInfoInput struct {
	// UserID 群友的QQ号
	UserID int64 `json:"user_id" jsonschema:"description=群友/私聊对象的QQ号"`
}

// GetUserInfoOutput 获取成员信息的输出
type GetUserInfoOutput struct {
	Success     bool     `json:"success"`
	Message     string   `json:"message,omitempty"`
	Nickname    string   `json:"nickname,omitempty"`
	SpeakStyle  string   `json:"speak_style,omitempty"`
	Interests   []string `json:"interests,omitempty"`
	CommonWords []string `json:"common_words,omitempty"`
	Activity    float64  `json:"activity,omitempty"` // 活跃度 0-1
	Intimacy    float64  `json:"intimacy,omitempty"` // 亲密度 0-1
	MsgCount    int      `json:"msg_count,omitempty"`
}

// getMemberInfoFunc 获取成员信息的实际实现
func getMemberInfoFunc(ctx context.Context, input *GetUserInfoInput) (*GetUserInfoOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &GetUserInfoOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	if input.UserID == 0 {
		return &GetUserInfoOutput{Success: false, Message: "用户 ID 不能为空"}, nil
	}

	profile, err := tc.MemoryMgr.GetMemberProfile(input.UserID)
	if err != nil {
		return &GetUserInfoOutput{
			Success: false,
			Message: "不太了解这个人",
		}, nil
	}

	var interests, commonWords []string
	if profile.Interests != "" {
		if err := sonic.UnmarshalString(profile.Interests, &interests); err != nil {
			zap.L().Warn("反序列化 interests 失败", zap.Error(err))
		}
	}
	if profile.CommonWords != "" {
		if err := sonic.UnmarshalString(profile.CommonWords, &commonWords); err != nil {
			zap.L().Warn("反序列化 commonWords 失败", zap.Error(err))
		}
	}

	return &GetUserInfoOutput{
		Success:     true,
		Nickname:    profile.Nickname,
		SpeakStyle:  profile.SpeakStyle,
		Interests:   interests,
		CommonWords: commonWords,
		Activity:    profile.Activity,
		Intimacy:    profile.Intimacy,
		MsgCount:    profile.MsgCount,
	}, nil
}

// NewGetMemberInfoTool 创建获取成员信息工具
func NewGetMemberInfoTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"getMemberInfo",
		"查看你对某个群友/私聊对象的了解。",
		getMemberInfoFunc,
	)
}
