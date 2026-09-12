package llm

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/memory"

	"github.com/cloudwego/eino-ext/components/embedding/openai"
)

// EmbeddingClient 向量嵌入客户端
type EmbeddingClient struct {
	client *openai.Embedder
}

// NewEmbeddingClient 创建 Embedding 客户端
func NewEmbeddingClient() (*EmbeddingClient, error) {
	cfg := config.Get()
	// 检查是否启用
	if !cfg.Embedding.Enabled {
		return nil, nil
	}

	ctx := context.Background()

	embedder, err := openai.NewEmbedder(ctx, &openai.EmbeddingConfig{
		BaseURL:    cfg.Embedding.BaseURL,
		APIKey:     cfg.Embedding.APIKey,
		Model:      cfg.Embedding.Model,
		Dimensions: &cfg.Memory.Milvus.VectorDim,
	})
	if err != nil {
		return nil, fmt.Errorf("创建 Embedder 失败: %w", err)
	}

	return &EmbeddingClient{
		client: embedder,
	}, nil
}

// Embed 生成文本的向量表示
func (c *EmbeddingClient) Embed(ctx context.Context, text string) ([]float64, error) {
	vectors, err := c.client.EmbedStrings(ctx, []string{text})
	if err != nil {
		return nil, err
	}

	if len(vectors) == 0 || len(vectors[0]) == 0 {
		return nil, fmt.Errorf("embedding 结果为空")
	}
	return vectors[0], nil
}

// 确保实现了接口
var _ memory.EmbeddingProvider = (*EmbeddingClient)(nil)
