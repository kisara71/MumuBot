package llm

import (
	"context"
	"fmt"
	"mumu-bot/internal/config"
	"sync"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/components/model"
)

var (
	defaultClient     model.ToolCallingChatModel
	defaultClientErr  error
	defaultClientOnce sync.Once

	auxClient     model.ToolCallingChatModel
	auxClientErr  error
	auxClientOnce sync.Once
)

// NewClient 创建 LLM 客户端（单例）
func NewClient() (model.ToolCallingChatModel, error) {
	defaultClientOnce.Do(func() {
		cfg := config.Get()
		ctx := context.Background()

		// 使用 Eino 的 OpenAI 兼容客户端
		chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL:     cfg.LLM.BaseURL,
			APIKey:      cfg.LLM.APIKey,
			Model:       cfg.LLM.Model,
			ExtraFields: cfg.LLM.ExtraFields,
		})
		if err != nil {
			defaultClientErr = fmt.Errorf("创建 ChatModel 失败: %w", err)
			return
		}

		defaultClient = chatModel
	})

	return defaultClient, defaultClientErr
}

// NewAuxClient 创建辅助 LLM 客户端（单例）
func NewAuxClient() (model.ToolCallingChatModel, error) {
	auxClientOnce.Do(func() {
		cfg := config.Get()
		ctx := context.Background()

		// 使用 AuxiliaryModel 配置
		chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
			BaseURL: cfg.AuxiliaryModel.BaseURL,
			APIKey:  cfg.AuxiliaryModel.APIKey,
			Model:   cfg.AuxiliaryModel.Model,
		})
		if err != nil {
			auxClientErr = fmt.Errorf("创建辅助 ChatModel 失败: %w", err)
			return
		}

		auxClient = chatModel
	})

	return auxClient, auxClientErr
}
