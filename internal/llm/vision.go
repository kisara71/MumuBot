package llm

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"
)

// VisionClient 多模态视觉模型客户端
type VisionClient struct {
	model *openai.ChatModel
}

// NewVisionClient 创建视觉模型客户端
func NewVisionClient() (*VisionClient, error) {
	cfg := config.Get().VisionLLM
	if !cfg.Enabled {
		return nil, nil
	}

	ctx := context.Background()
	model, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL: cfg.BaseURL,
		APIKey:  cfg.APIKey,
		Model:   cfg.Model,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 VisionModel 失败: %w", err)
	}

	return &VisionClient{
		model: model,
	}, nil
}

// DescribeImage 描述图片内容
func (v *VisionClient) DescribeImage(ctx context.Context, imageURL string) (string, error) {
	if v == nil || v.model == nil {
		return "[图片]", nil
	}

	// 构建多模态消息
	msg := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{
				Type: schema.ChatMessagePartTypeImageURL,
				Image: &schema.MessageInputImage{
					MessagePartCommon: schema.MessagePartCommon{
						URL: &imageURL,
					},
					Detail: schema.ImageURLDetailHigh,
				},
			},
			{
				Type: schema.ChatMessagePartTypeText,
				Text: "请用中文尽可能地描述这张图片的内容和内涵，输出一段平文本，300字以内。如果是表情包请重点描述表情、情绪、文字。若画面中有文字内容，请概括描述。若画面中有明确角色（例如动漫/游戏/电影人物），请补充说明角色名称和出处（若能判断）、当前情绪状态、整体风格或用途。",
			},
		},
	}

	resp, err := v.model.Generate(ctx, []*schema.Message{msg})
	if err != nil {
		return "[图片:识别失败]", nil
	}

	desc := strings.TrimSpace(resp.Content)
	if desc == "" {
		return "[图片]", nil
	}
	return fmt.Sprintf("[图片:%s]", desc), nil
}

// DescribeVideo 描述视频内容
func (v *VisionClient) DescribeVideo(ctx context.Context, videoURL string) (string, error) {
	if v == nil || v.model == nil {
		return "[视频]", nil
	}

	msg := &schema.Message{
		Role: schema.User,
		UserInputMultiContent: []schema.MessageInputPart{
			{
				Type: schema.ChatMessagePartTypeVideoURL,
				Video: &schema.MessageInputVideo{
					MessagePartCommon: schema.MessagePartCommon{
						URL: &videoURL,
					},
				},
			},
			{
				Type: schema.ChatMessagePartTypeText,
				Text: "请用中文尽可能地描述这段视频的内容和内涵，输出一段平文本，300字以内。若能判断角色、情绪或关键事件、物体，请一并说明。",
			},
		},
	}

	resp, err := v.model.Generate(ctx, []*schema.Message{msg})
	if err != nil {
		return "[视频:识别失败]", nil
	}

	desc := strings.TrimSpace(resp.Content)
	if desc == "" {
		return "[视频]", nil
	}
	return fmt.Sprintf("[视频:%s]", desc), nil
}
