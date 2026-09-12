package tools

import (
	"context"

	mutils "github.com/kisara71/luma/internal/utils"

	"github.com/bytedance/sonic"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"go.uber.org/zap"
)

// ==================== 更新用户画像工具 ====================

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

// UpdateUserProfileInput 更新用户画像的输入参数
type UpdateUserProfileInput struct {
	// Alias 对对方的称呼/别名
	Alias []string `json:"alias,omitempty" jsonschema:"description=你对对方的称呼、外号或约定别名（只传新增项）"`
	// SpeakStyle 说话风格描述
	SpeakStyle string `json:"speak_style,omitempty" jsonschema:"description=说话风格描述（覆盖之前的描述）"`
	// Interests 兴趣爱好列表
	Interests []string `json:"interests,omitempty" jsonschema:"description=兴趣爱好列表（只传入新增的项）"`
	// CommonWords 常用词汇或口头禅
	CommonWords []string `json:"common_words,omitempty" jsonschema:"description=常用词汇或口头禅（只传入新增的项）"`
	// IntimacyDelta 亲密度变化值 -0.3 到 0.3
	IntimacyDelta float64 `json:"intimacy_delta,omitempty" jsonschema:"minimum=-0.3,maximum=0.3,description=亲密度变化值(-0.3到0.3)，正数表示增加亲密度，负数表示降低亲密度"`
	// TrustDelta 信任度变化值
	TrustDelta float64 `json:"trust_delta,omitempty" jsonschema:"minimum=-0.3,maximum=0.3,description=信任度变化值(-0.3到0.3)"`
	// FamiliarityDelta 熟悉度变化值
	FamiliarityDelta float64 `json:"familiarity_delta,omitempty" jsonschema:"minimum=-0.3,maximum=0.3,description=熟悉度变化值(-0.3到0.3)"`
	// RespectDelta 认可度变化值
	RespectDelta float64 `json:"respect_delta,omitempty" jsonschema:"minimum=-0.3,maximum=0.3,description=认可度变化值(-0.3到0.3)"`
}

// UpdateUserProfileOutput 更新用户画像的输出
type UpdateUserProfileOutput struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// updateUserProfileFunc 更新用户画像的实际实现
func updateUserProfileFunc(ctx context.Context, input *UpdateUserProfileInput) (*UpdateUserProfileOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &UpdateUserProfileOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	// Private-only: the model cannot select or mutate another user's profile.
	userID := tc.SessionRef().UserID

	profile, err := tc.MemoryMgr.GetUserProfile(userID)
	if err != nil {
		return &UpdateUserProfileOutput{Success: false, Message: err.Error()}, nil
	}

	if input.SpeakStyle != "" {
		profile.SpeakStyle = input.SpeakStyle
	}
	if len(input.Alias) > 0 {
		var existingAlias []string
		if profile.Alias != "" {
			if err := sonic.UnmarshalString(profile.Alias, &existingAlias); err != nil {
				existingAlias = []string{}
			}
		}
		mergedAlias := mergeAndDeduplicateStrings(existingAlias, input.Alias)
		b, _ := sonic.MarshalString(mergedAlias)
		profile.Alias = b
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
	profile.Trust = mutils.ClampFloat64(profile.Trust+input.TrustDelta, 0, 1)
	profile.Familiarity = mutils.ClampFloat64(profile.Familiarity+input.FamiliarityDelta, 0, 1)
	profile.Respect = mutils.ClampFloat64(profile.Respect+input.RespectDelta, 0, 1)

	if err := tc.MemoryMgr.UpdateUserProfile(profile); err != nil {
		return &UpdateUserProfileOutput{Success: false, Message: err.Error()}, nil
	}

	return &UpdateUserProfileOutput{Success: true, Message: "已更新对该用户的了解"}, nil
}

// NewUpdateUserProfileTool 创建更新用户画像工具
func NewUpdateUserProfileTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"updateUserProfile",
		"更新当前聊天对象的画像。当发现稳定的新特点、称呼或兴趣时使用；普通闲聊不要调用。user_id 会被系统绑定为当前对方。",
		updateUserProfileFunc,
	)
}

// ==================== 获取用户信息工具 ====================

// GetUserInfoInput 获取用户信息的输入参数
type GetUserInfoInput struct {
}

// GetUserInfoOutput 获取用户信息的输出
type GetUserInfoOutput struct {
	Success     bool     `json:"success"`
	Message     string   `json:"message,omitempty"`
	Nickname    string   `json:"nickname,omitempty"`
	Alias       []string `json:"alias,omitempty"`
	SpeakStyle  string   `json:"speak_style,omitempty"`
	Interests   []string `json:"interests,omitempty"`
	CommonWords []string `json:"common_words,omitempty"`
	Activity    float64  `json:"activity,omitempty"` // 活跃度 0-1
	Intimacy    float64  `json:"intimacy,omitempty"` // 亲密度 0-1
	Trust       float64  `json:"trust,omitempty"`
	Familiarity float64  `json:"familiarity,omitempty"`
	Respect     float64  `json:"respect,omitempty"`
	MsgCount    int      `json:"msg_count,omitempty"`
}

// getUserInfoFunc 获取用户信息的实际实现
func getUserInfoFunc(ctx context.Context, input *GetUserInfoInput) (*GetUserInfoOutput, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return &GetUserInfoOutput{Success: false, Message: "工具上下文未初始化"}, nil
	}

	profile, err := tc.MemoryMgr.GetUserProfile(tc.SessionRef().UserID)
	if err != nil {
		return &GetUserInfoOutput{
			Success: false,
			Message: "不太了解这个人",
		}, nil
	}

	var aliases, interests, commonWords []string
	if profile.Alias != "" {
		if err := sonic.UnmarshalString(profile.Alias, &aliases); err != nil {
			zap.L().Warn("反序列化 alias 失败", zap.Error(err))
		}
	}
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
		Alias:       aliases,
		SpeakStyle:  profile.SpeakStyle,
		Interests:   interests,
		CommonWords: commonWords,
		Activity:    profile.Activity,
		Intimacy:    profile.Intimacy,
		Trust:       profile.Trust,
		Familiarity: profile.Familiarity,
		Respect:     profile.Respect,
		MsgCount:    profile.MsgCount,
	}, nil
}

// NewGetUserInfoTool 创建获取用户信息工具
func NewGetUserInfoTool() (tool.InvokableTool, error) {
	return utils.InferTool(
		"getUserProfile",
		"查看当前聊天对象的画像。user_id 会被系统绑定为当前对方。",
		getUserInfoFunc,
	)
}
