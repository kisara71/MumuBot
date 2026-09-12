package onebot

import (
	"context"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/session"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Client OneBot WebSocket客户端
type Client struct {
	conn      *websocket.Conn
	connMu    sync.Mutex
	selfID    atomic.Int64
	ctx       context.Context
	cancel    context.CancelFunc
	closeOnce sync.Once

	// 消息回调
	onMessage   func(*Message)
	onMessageMu sync.RWMutex

	// 重连控制
	reconnecting atomic.Bool

	// API 调用响应等待
	echoCounter atomic.Uint64
	pendingReqs sync.Map // map[string]chan *APIResponse
}

// APIResponse OneBot API 响应
type APIResponse struct {
	Status  string      `json:"status"`  // ok / failed
	RetCode int         `json:"retcode"` // 0 表示成功
	Data    interface{} `json:"data"`    // 可以是 map 或 array
	Echo    string      `json:"echo"`
	Message string      `json:"message,omitempty"` // 错误信息
}

// DataMap 获取响应数据为 map 类型（用于普通 API）
func (r *APIResponse) DataMap() map[string]interface{} {
	if r.Data == nil {
		return nil
	}
	if m, ok := r.Data.(map[string]interface{}); ok {
		return m
	}
	return nil
}

// Message 私聊消息。
type Message struct {
	MessageID      int64            `json:"message_id"`
	ConversationID string           `json:"conversation_id,omitempty"`
	UserID         int64            `json:"user_id"`
	Nickname       string           `json:"nickname"`
	Content        string           `json:"content"`                 // 纯文本内容
	Time           time.Time        `json:"time"`                    // 消息时间
	Images         []ImageInfo      `json:"images,omitempty"`        // 图片列表
	Videos         []VideoInfo      `json:"videos,omitempty"`        // 视频列表
	Faces          []FaceInfo       `json:"faces,omitempty"`         // 表情列表
	AtList         []int64          `json:"at_list,omitempty"`       // @的用户列表
	Reply          *ReplyInfo       `json:"reply,omitempty"`         // 回复信息
	Forwards       []ForwardMessage `json:"forwards,omitempty"`      // 合并转发内容
	FinalContent   string           `json:"final_content,omitempty"` // 处理后的最终内容
}

// ImageInfo 图片信息
type ImageInfo struct {
	URL     string `json:"url"`
	File    string `json:"file"`
	Summary string `json:"summary,omitempty"` // 图片摘要/描述
	SubType int    `json:"sub_type"`          // 0普通图片 1表情包
}

// VideoInfo 视频信息
type VideoInfo struct {
	URL  string `json:"url"`
	File string `json:"file"`
}

// FaceInfo 表情信息
type FaceInfo struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ReplyInfo 回复信息
type ReplyInfo struct {
	MessageID int64  `json:"message_id"`
	Content   string `json:"content,omitempty"`   // 被回复消息内容
	SenderID  int64  `json:"sender_id,omitempty"` // 被回复消息发送者 ID
	Nickname  string `json:"nickname,omitempty"`  // 被回复消息发送者昵称
}

// ForwardMessage 合并转发中的单条消息
type ForwardMessage struct {
	UserID   int64     `json:"user_id"`
	Nickname string    `json:"nickname"`
	Time     time.Time `json:"time"`
	Content  string    `json:"content"`
}

// CardMessage 卡片消息解析结果
type CardMessage struct {
	App   string `json:"app"`   // 应用标识
	Title string `json:"title"` // 标题
	Desc  string `json:"desc"`  // 描述
	URL   string `json:"url"`   // 链接
}

// Format 格式化卡片消息为可读文本
func (c *CardMessage) Format() string {
	if c.URL != "" {
		return fmt.Sprintf("[卡片:%s - %s 链接:%s]", c.Title, c.Desc, c.URL)
	}
	if c.Desc != "" {
		return fmt.Sprintf("[卡片:%s - %s]", c.Title, c.Desc)
	}
	return fmt.Sprintf("[卡片:%s]", c.Title)
}

// LoginInfo 登录信息
type LoginInfo struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
}

// NewClient 创建OneBot客户端
func NewClient() *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{
		ctx:    ctx,
		cancel: cancel,
	}
}

// Connect 连接到OneBot服务
func (c *Client) Connect() error {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	cfg := config.Get()
	header := make(map[string][]string)
	if cfg.OneBot.AccessToken != "" {
		header["Authorization"] = []string{"Bearer " + cfg.OneBot.AccessToken}
	}

	conn, _, err := websocket.DefaultDialer.Dial(cfg.OneBot.WsURL, header)
	if err != nil {
		return fmt.Errorf("WebSocket连接失败: %w", err)
	}

	c.conn = conn
	c.reconnecting.Store(false)

	// 启动消息接收循环
	go c.receiveLoop(conn)

	zap.L().Info("已连接到 OneBot", zap.String("url", cfg.OneBot.WsURL))
	return nil
}

// receiveLoop 消息接收循环
func (c *Client) receiveLoop(conn *websocket.Conn) {
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}

		_, message, err := conn.ReadMessage()
		if err != nil {
			zap.L().Error("读取消息失败", zap.Error(err))
			c.handleDisconnect()
			return
		}

		go c.handleMessage(message)
	}
}

// handleMessage 处理收到的消息
func (c *Client) handleMessage(data []byte) {
	var event map[string]interface{}
	if err := sonic.Unmarshal(data, &event); err != nil {
		zap.L().Error("解析消息失败", zap.Error(err))
		return
	}

	// 检查是否是 API 响应（有 echo 字段）
	if echo, ok := event["echo"].(string); ok && echo != "" {
		c.handleAPIResponse(event, echo)
		return
	}

	// 处理事件
	if postType, ok := event["post_type"].(string); ok {
		switch postType {
		case "meta_event":
			c.handleMetaEvent(event)
		case "message":
			c.handleMessageEvent(event)
		}
	}
}

// handleAPIResponse 处理 API 响应
func (c *Client) handleAPIResponse(event map[string]interface{}, echo string) {
	if ch, ok := c.pendingReqs.Load(echo); ok {
		resp := &APIResponse{Echo: echo}
		if status, ok := event["status"].(string); ok {
			resp.Status = status
		}
		if retCode, ok := parseInt(event["retcode"]); ok {
			resp.RetCode = retCode
		}
		// Data 可能是 map 或 array
		resp.Data = event["data"]
		if msg, ok := event["message"].(string); ok {
			resp.Message = msg
		}
		select {
		case ch.(chan *APIResponse) <- resp:
		default:
			zap.L().Debug("忽略重复或过期的 API 响应", zap.String("echo", echo))
		}
	}
}

// handleMetaEvent 处理元事件
func (c *Client) handleMetaEvent(event map[string]interface{}) {
	metaType, _ := event["meta_event_type"].(string)

	if metaType == "lifecycle" {
		subType, _ := event["sub_type"].(string)
		if subType == "connect" {
			if selfID, ok := parseInt64(event["self_id"]); ok {
				c.selfID.Store(selfID)
				zap.L().Info("Bot 已上线", zap.Int64("qq", c.selfID.Load()))
			}
		}
	}
}

// handleMessageEvent 处理消息事件
func (c *Client) handleMessageEvent(event map[string]interface{}) {
	msgSrc, ok := event["message_type"].(string)
	if !ok {
		zap.L().Error("预期之外的信息类型:", zap.Any("msg_type", event["message_type"]))
		return
	}

	if msgSrc != "private" {
		// Luma deliberately ignores group events at the transport boundary.
		return
	}
	msg := c.parseUserMessage(event)
	// 解析消息
	if msg == nil {
		return
	}

	// 调用消息回调
	c.onMessageMu.RLock()
	handler := c.onMessage
	c.onMessageMu.RUnlock()
	if handler != nil {
		handler(msg)
	}
}

func (c *Client) parseUserMessage(event map[string]interface{}) *Message {
	msg := &Message{}
	//	commmon message
	c.parseCommonMessage(event, msg)

	// 私聊会话对象
	if userID, ok := parseInt64(event["user_id"]); ok && userID > 0 {
		msg.ConversationID = session.NewRef(userID).ID()
	}

	// 发送者信息
	if sender, ok := event["sender"].(map[string]interface{}); ok {
		if userID, ok := parseInt64(sender["user_id"]); ok {
			msg.UserID = userID
		}
		if nickname, ok := sender["nickname"].(string); ok {
			msg.Nickname = nickname
		}
	}
	c.parseMessageSegments(event, msg)

	return msg
}
func (c *Client) parseCommonMessage(event map[string]interface{}, msg *Message) {
	// 消息时间
	if t, ok := parseInt64(event["time"]); ok {
		msg.Time = time.Unix(t, 0)
	} else {
		msg.Time = time.Now()
	}

	// 消息 ID
	if msgID, ok := parseInt64(event["message_id"]); ok {
		msg.MessageID = msgID
		readCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
		if err := c.MarkMsgAsRead(readCtx, msgID); err != nil {
			zap.L().Error("标记消息已读失败", zap.Error(err))
		}
		cancel()
	}
}

// parseMessageSegments 解析消息段，填充消息各字段
func (c *Client) parseMessageSegments(event map[string]interface{}, msg *Message) {
	message, ok := event["message"].([]interface{})
	if !ok {
		if raw, ok := event["raw_message"].(string); ok {
			msg.Content = raw
		}
		return
	}

	var textParts []string

	for _, seg := range message {
		segMap, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}

		segType, _ := segMap["type"].(string)
		data, _ := segMap["data"].(map[string]interface{})
		if data == nil {
			continue
		}

		switch segType {
		case "text":
			if t, ok := data["text"].(string); ok {
				textParts = append(textParts, t)
			}

		case "image":
			img := ImageInfo{}
			if url, ok := data["url"].(string); ok {
				img.URL = url
			}
			if file, ok := data["file"].(string); ok {
				img.File = file
			}
			if summary, ok := data["summary"].(string); ok {
				img.Summary = summary
			}
			if subType, ok := parseInt(data["sub_type"]); ok {
				img.SubType = subType
			}
			if img.URL != "" || img.File != "" {
				msg.Images = append(msg.Images, img)
			}

		case "face":
			face := FaceInfo{}
			// ID
			if id, ok := parseInt(data["id"]); ok {
				face.ID = id
			}
			// 表情名称（NapCat 扩展字段）
			if name, ok := data["name"].(string); ok && name != "" {
				face.Name = name
			} else if text, ok := data["text"].(string); ok && text != "" {
				face.Name = text
			} else if raw, ok := data["raw"].(string); ok && raw != "" {
				face.Name = raw
			}
			msg.Faces = append(msg.Faces, face)

		case "at":
			if qq, ok := data["qq"].(string); ok {
				if qq == "all" {
					textParts = append(textParts, "@全体成员")
				} else if qqID, err := strconv.ParseInt(qq, 10, 64); err == nil {
					msg.AtList = append(msg.AtList, qqID)
					textParts = append(textParts, "@"+qq)
				}
			} else if qqID, ok := parseInt64(data["qq"]); ok {
				msg.AtList = append(msg.AtList, qqID)
				textParts = append(textParts, fmt.Sprintf("@%d", qqID))
			}

		case "reply":
			if replyMsgID, ok := parseInt64(data["id"]); ok {
				msg.Reply = &ReplyInfo{MessageID: replyMsgID}
				// 同步获取被回复消息内容
				replyCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
				replyData, err := c.GetMsg(replyCtx, replyMsgID)
				cancel()
				if err == nil && replyData != nil {
					if rawMsg, ok := replyData["raw_message"].(string); ok {
						msg.Reply.Content = rawMsg
					}
					if sender, ok := replyData["sender"].(map[string]interface{}); ok {
						if uid, ok := parseInt64(sender["user_id"]); ok {
							msg.Reply.SenderID = uid
						}
						if nick, ok := sender["nickname"].(string); ok {
							msg.Reply.Nickname = nick
						}
					}
				}
			}

		case "mface": // 商城表情/魔法表情
			img := ImageInfo{}
			if url, ok := data["url"].(string); ok {
				img.URL = url
			}
			if summary, ok := data["summary"].(string); ok {
				img.Summary = summary
			}
			img.SubType = 1 // 标记为表情包类型
			if img.URL != "" {
				msg.Images = append(msg.Images, img)
			}

		case "record": // 语音消息
			textParts = append(textParts, "[语音]")

		case "video": // 视频消息
			vid := VideoInfo{}
			if url, ok := data["url"].(string); ok {
				vid.URL = url
			}
			if file, ok := data["file"].(string); ok {
				vid.File = file
			}
			if vid.URL != "" || vid.File != "" {
				msg.Videos = append(msg.Videos, vid)
			}

		case "file": // 文件
			if name, ok := data["name"].(string); ok {
				textParts = append(textParts, fmt.Sprintf("[文件:%s]", name))
			} else {
				textParts = append(textParts, "[文件]")
			}

		case "json": // JSON 卡片消息
			if jsonStr, ok := data["data"].(string); ok {
				card := parseCardMessage(jsonStr)
				if card != nil {
					textParts = append(textParts, card.Format())
				} else {
					textParts = append(textParts, "[卡片消息]")
				}
			} else {
				textParts = append(textParts, "[卡片消息]")
			}

		case "forward": // 合并转发
			if forwardID, ok := data["id"].(string); ok {
				forwardCtx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
				nodes, err := c.GetForwardMsg(forwardCtx, forwardID)
				cancel()
				if err == nil && len(nodes) > 0 {
					// 仅显示前四条，每条限制20个rune
					limit := 4
					if len(nodes) < limit {
						limit = len(nodes)
					}
					var parts []string
					for i := 0; i < limit; i++ {
						node := nodes[i]
						content := "[消息]"
						if node.Content != "" {
							runes := []rune(node.Content)
							if len(runes) > 20 {
								content = string(runes[:20]) + "..."
							} else {
								content = node.Content
							}
						}
						parts = append(parts, fmt.Sprintf("%s(%d):%s", node.Nickname, node.UserID, content))
					}
					msg.Forwards = nodes
					summary := fmt.Sprintf("[合并转发，共%d条，预览：%s] ", len(nodes), strings.Join(parts, " / "))
					textParts = append(textParts, summary)
					continue
				}
			}
			textParts = append(textParts, "[合并转发]")
		}
	}

	// 合并文本内容
	for i, part := range textParts {
		if i > 0 {
			msg.Content += " "
		}
		msg.Content += part
	}
}

// OnMessage 设置消息回调
func (c *Client) OnMessage(handler func(*Message)) {
	c.onMessageMu.Lock()
	defer c.onMessageMu.Unlock()
	c.onMessage = handler
}

// SendPrivateMessage 发送私聊消息
func (c *Client) SendPrivateMessage(ctx context.Context, userID int64, content string) (int64, error) {
	resp, err := c.callAPI(ctx, "send_private_msg", map[string]interface{}{
		"user_id": userID,
		"message": content,
	})
	if err != nil {
		return 0, err
	}
	if data := resp.DataMap(); data != nil {
		if msgID, ok := parseInt64(data["message_id"]); ok {
			return msgID, nil
		}
	}
	return 0, nil
}

// DeleteMsg 撤回消息
func (c *Client) DeleteMsg(ctx context.Context, messageID int64) error {
	_, err := c.callAPI(ctx, "delete_msg", map[string]interface{}{
		"message_id": strconv.FormatInt(messageID, 10),
	})
	return err
}

// GetMsg 获取消息详情
func (c *Client) GetMsg(ctx context.Context, messageID int64) (map[string]interface{}, error) {
	resp, err := c.callAPI(ctx, "get_msg", map[string]interface{}{
		"message_id": messageID,
	})
	if err != nil {
		return nil, err
	}
	return resp.DataMap(), nil
}

// GetLoginInfo 获取登录号信息
func (c *Client) GetLoginInfo(ctx context.Context) (*LoginInfo, error) {
	resp, err := c.callAPI(ctx, "get_login_info", nil)
	if err != nil {
		return nil, err
	}
	data := resp.DataMap()
	if data == nil {
		return nil, fmt.Errorf("无效的响应数据")
	}
	info := &LoginInfo{}
	if userID, ok := parseInt64(data["user_id"]); ok {
		info.UserID = userID
	}
	if nickname, ok := data["nickname"].(string); ok {
		info.Nickname = nickname
	}
	return info, nil
}

func (c *Client) MarkMsgAsRead(ctx context.Context, messageID int64) error {
	_, err := c.callAPI(ctx, "mark_msg_as_read", map[string]interface{}{"message_id": messageID})
	return err
}

func (c *Client) callAPI(ctx context.Context, action string, params map[string]interface{}) (*APIResponse, error) {
	if ctx == nil {
		ctx = c.ctx
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	echo := fmt.Sprintf("%d", c.echoCounter.Add(1))

	// 创建响应通道
	respCh := make(chan *APIResponse, 1)
	c.pendingReqs.Store(echo, respCh)
	defer c.pendingReqs.Delete(echo)

	// 发送请求
	c.connMu.Lock()
	if c.conn == nil {
		c.connMu.Unlock()
		return nil, fmt.Errorf("未连接到 OneBot 服务")
	}

	req := map[string]interface{}{
		"action": action,
		"params": params,
		"echo":   echo,
	}
	data, err := sonic.Marshal(req)
	if err != nil {
		c.connMu.Unlock()
		return nil, err
	}

	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.connMu.Unlock()
		return nil, err
	}
	c.connMu.Unlock()

	// 等待响应（带超时）
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		if resp.RetCode != 0 {
			return resp, fmt.Errorf("API调用失败[%d]: %s", resp.RetCode, resp.Message)
		}
		return resp, nil
	}
}

// handleDisconnect 处理断开连接
func (c *Client) handleDisconnect() {
	if !c.reconnecting.CompareAndSwap(false, true) {
		return
	}

	zap.L().Warn("连接断开，尝试重连...")

	interval := time.Duration(config.Get().OneBot.ReconnectInterval) * time.Second
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-time.After(interval):
		}

		if err := c.Connect(); err == nil {
			zap.L().Info("重连成功")
			return
		}
		zap.L().Warn("重连失败，继续尝试...")
	}
}

// GetSelfID 获取Bot的QQ号
func (c *Client) GetSelfID() int64 {
	return c.selfID.Load()
}

// Close 关闭连接
func (c *Client) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		if c.cancel != nil {
			c.cancel()
		}

		c.connMu.Lock()
		defer c.connMu.Unlock()

		if c.conn != nil {
			closeErr = c.conn.Close()
			c.conn = nil
		}
	})
	return closeErr
}

// parseCardMessage 解析JSON卡片消息
func parseCardMessage(jsonStr string) *CardMessage {
	var data map[string]interface{}
	if err := sonic.UnmarshalString(jsonStr, &data); err != nil {
		return nil
	}

	card := &CardMessage{}

	// 获取 app 类型
	if app, ok := data["app"].(string); ok {
		card.App = app
	}

	// 尝试从 meta 中提取信息（常见结构）
	if meta, ok := data["meta"].(map[string]interface{}); ok {
		// 遍历 meta 中的第一个子对象
		for _, v := range meta {
			if detail, ok := v.(map[string]interface{}); ok {
				if title, ok := detail["title"].(string); ok {
					card.Title = title
				}
				if desc, ok := detail["desc"].(string); ok {
					card.Desc = desc
				}
				if jumpUrl, ok := detail["jumpUrl"].(string); ok {
					card.URL = jumpUrl
				} else if qqdocurl, ok := detail["qqdocurl"].(string); ok {
					card.URL = qqdocurl
				}
				break
			}
		}
	}

	// 尝试从 prompt 获取标题（备用）
	if card.Title == "" {
		if prompt, ok := data["prompt"].(string); ok {
			card.Title = prompt
		}
	}

	// 尝试从 desc 获取描述（备用）
	if card.Desc == "" {
		if desc, ok := data["desc"].(string); ok {
			card.Desc = desc
		}
	}

	if card.Title == "" && card.Desc == "" {
		return nil
	}

	return card
}

// GetForwardMsg 获取合并转发消息内容
func (c *Client) GetForwardMsg(ctx context.Context, forwardID string) ([]ForwardMessage, error) {
	if forwardID == "" {
		return nil, nil
	}
	resp, err := c.callAPI(ctx, "get_forward_msg", map[string]interface{}{
		"message_id": forwardID,
	})
	if err != nil {
		return nil, err
	}
	data := resp.DataMap()
	if data == nil {
		return nil, nil
	}
	return parseForwardMessages(data), nil
}

func parseForwardMessages(data map[string]interface{}) []ForwardMessage {
	var rawMsgs []interface{}
	if msgs, ok := data["messages"].([]interface{}); ok {
		rawMsgs = msgs
	} else if msgs, ok := data["message"].([]interface{}); ok {
		rawMsgs = msgs
	}
	if len(rawMsgs) == 0 {
		return nil
	}

	var result []ForwardMessage
	for _, item := range rawMsgs {
		msgMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		fm := ForwardMessage{}
		if sender, ok := msgMap["sender"].(map[string]interface{}); ok {
			if uid, ok := parseInt64(sender["user_id"]); ok {
				fm.UserID = uid
			}
			if nick, ok := sender["nickname"].(string); ok {
				fm.Nickname = nick
			}
		}
		if t, ok := parseInt64(msgMap["time"]); ok {
			fm.Time = time.Unix(t, 0)
		}
		if segs, ok := msgMap["message"].([]interface{}); ok {
			fm.Content = extractTextFromSegments(segs)
		} else if content, ok := msgMap["content"].(string); ok {
			fm.Content = content
		}
		if fm.Content == "" {
			fm.Content = "[消息]"
		}
		result = append(result, fm)
	}
	return result
}

// extractTextFromSegments 从消息段中提取文本内容
func extractTextFromSegments(segments []interface{}) string {
	var parts []string
	for _, seg := range segments {
		segMap, ok := seg.(map[string]interface{})
		if !ok {
			continue
		}
		segType, _ := segMap["type"].(string)
		data, _ := segMap["data"].(map[string]interface{})
		if data == nil {
			continue
		}
		switch segType {
		case "text":
			if t, ok := data["text"].(string); ok {
				parts = append(parts, t)
			}
		case "image":
			parts = append(parts, "[图片]")
		case "face":
			parts = append(parts, "[表情]")
		case "record":
			parts = append(parts, "[语音]")
		case "video":
			parts = append(parts, "[视频]")
		case "file":
			parts = append(parts, "[文件]")
		case "at":
			if qq, ok := data["qq"].(string); ok {
				if qq == "all" {
					parts = append(parts, "@全体成员")
				} else {
					parts = append(parts, "@"+qq)
				}
			} else if qqID, ok := parseInt64(data["qq"]); ok {
				parts = append(parts, fmt.Sprintf("@%d", qqID))
			}
		case "json":
			parts = append(parts, "[卡片消息]")
		case "forward":
			parts = append(parts, "[合并转发]")
		}
	}
	return strings.Join(parts, "")
}

// SendPrivateImageMessage 发送私聊图片/表情包消息。
func (c *Client) SendPrivateImageMessage(ctx context.Context, userID int64, filePath string, isSticker bool) (int64, error) {
	return c.sendImageMessage(ctx, "send_private_msg", "user_id", userID, filePath, isSticker)
}

func (c *Client) sendImageMessage(ctx context.Context, action string, target string, targetID int64, filePath string, isSticker bool) (int64, error) {
	subType := 0
	if isSticker {
		subType = 1
	}

	message := []map[string]interface{}{
		{
			"type": "image",
			"data": map[string]interface{}{
				"file":     "file:///" + filePath,
				"sub_type": subType,
			},
		},
	}

	resp, err := c.callAPI(ctx, action, map[string]interface{}{
		target:    targetID,
		"message": message,
	})
	if err != nil {
		return 0, err
	}
	if data := resp.DataMap(); data != nil {
		if msgID, ok := parseInt64(data["message_id"]); ok {
			return msgID, nil
		}
	}
	return 0, nil
}

// 助手函数
func parseInt64(v interface{}) (int64, bool) {
	if v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case int64:
		return val, true
	case float64:
		return int64(val), true
	case int:
		return int64(val), true
	case string:
		i, err := strconv.ParseInt(val, 10, 64)
		return i, err == nil
	}
	return 0, false
}

func parseInt(v interface{}) (int, bool) {
	if v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case int:
		return val, true
	case float64:
		return int(val), true
	case int64:
		return int(val), true
	case string:
		i, err := strconv.Atoi(val)
		return i, err == nil
	}
	return 0, false
}
