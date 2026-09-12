# Luma

面向即时通讯场景的自主 AI Agent，支持角色设定、长期记忆、主动交互和工具调用。

## 功能

- 独立会话与用户记忆
- 主动联系与消息合并
- 长期记忆、用户画像和情绪状态
- 图片、视频和表情包处理
- OpenAI 兼容模型
- MCP 工具扩展
- Docker 部署

## 平台

当前提供 OneBot 11 私聊接入。其他消息平台将在接入层抽象完成后支持。

## 本地运行

依赖 Go 1.25、MySQL 和已支持平台的消息网关。语义记忆额外依赖 Milvus。

```bash
cp config/config.example.yaml config/config.yaml
cp config/mcp.example.json config/mcp.json
go run .
```

环境变量：

- `LUMA_LLM_API_KEY`
- `LUMA_EMBEDDING_API_KEY`
- `LUMA_VISION_API_KEY`
- `LUMA_ONEBOT_TOKEN`
- `LUMA_MYSQL_PASSWORD`

## Docker

```bash
cp config/config.docker.example.yaml config/config.yaml
cp config/mcp.example.json config/mcp.json
cp .env.example .env
docker compose up -d --build
```

默认启动 Luma 和 MySQL。启用 `embedding.enabled` 后使用向量服务：

```bash
docker compose --profile vector up -d --build
```

健康检查：

```bash
curl http://127.0.0.1:8080/health
```

## MCP

在 `config/mcp.json` 中配置 stdio、SSE 或 Streamable HTTP 服务。搜索工具示例需要 `BRAVE_API_KEY`。

## License

[MIT](LICENSE)
