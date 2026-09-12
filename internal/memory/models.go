package memory

import (
	"strings"
	"time"

	"github.com/bytedance/sonic"
)

// MemoryType 记忆类型
type MemoryType string

const (
	MemoryTypeUserFact       MemoryType = "user_fact"       // 某个用户的稳定事实，尤其适合私聊沉淀
	MemoryTypeSelfExperience MemoryType = "self_experience" // 自身经历（参与的事、被提及、感受等）
	MemoryTypeConversation   MemoryType = "conversation"    // 对话中的重要信息或阶段性上下文
	MemoryTypeExpression     MemoryType = "expression"      // 这个用户特有的词、梗、缩写及使用语境
)

// Memory 是绑定到单个私聊对象的长期记忆。
type Memory struct {
	ID             uint       `gorm:"primarykey" json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ConversationID string     `gorm:"type:varchar(32);index" json:"conversation_id"`
	Type           MemoryType `gorm:"type:varchar(50);index" json:"type"`
	UserID         int64      `gorm:"index" json:"user_id,omitempty"`
	Content        string     `gorm:"type:text" json:"content"`
	Importance     float64    `gorm:"default:0.5" json:"importance"`
	AccessCount    int        `gorm:"default:0" json:"access_count"`
}

func (Memory) TableName() string { return "memories" }

// UserProfile 用户画像
type UserProfile struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	UserID      int64     `gorm:"uniqueIndex:idx_user" json:"user_id"`
	Nickname    string    `gorm:"type:varchar(100)" json:"nickname"` //	qq 昵称
	Alias       string    `gorm:"type:text" json:"alias"`            // bot与user约定/bot主动起的名字(JSON array)
	SpeakStyle  string    `gorm:"type:text" json:"speak_style"`
	Interests   string    `gorm:"type:text" json:"interests"`
	CommonWords string    `gorm:"type:text" json:"common_words"`
	Activity    float64   `gorm:"default:0.5" json:"activity"`
	Intimacy    float64   `gorm:"default:0.3" json:"intimacy"`
	Trust       float64   `gorm:"default:0.4" json:"trust"`
	Familiarity float64   `gorm:"default:0.2" json:"familiarity"`
	Respect     float64   `gorm:"default:0.5" json:"respect"`
	LastSpeak   time.Time `json:"last_speak"`
	MsgCount    int       `gorm:"default:0" json:"msg_count"`
}

func (UserProfile) TableName() string { return "user_profiles" }

func (p *UserProfile) AliasList() []string {
	if p == nil || strings.TrimSpace(p.Alias) == "" {
		return nil
	}

	var aliases []string
	if err := sonic.UnmarshalString(p.Alias, &aliases); err != nil {
		return nil
	}

	result := make([]string, 0, len(aliases))
	seen := make(map[string]struct{}, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		result = append(result, alias)
	}
	return result
}

func (p *UserProfile) PreferredName(fallback string) string {
	if p != nil {
		if aliases := p.AliasList(); len(aliases) > 0 {
			return aliases[0]
		}
		if name := strings.TrimSpace(p.Nickname); name != "" {
			return name
		}
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		return fallback
	}
	return ""
}

// MessageLog 消息日志
type MessageLog struct {
	ID              uint      `gorm:"primarykey" json:"id"`
	CreatedAt       time.Time `gorm:"index" json:"created_at"`
	MessageID       string    `gorm:"type:varchar(100);uniqueIndex" json:"message_id"`
	ConversationID  string    `gorm:"type:varchar(32);index" json:"conversation_id"`
	UserID          int64     `gorm:"index" json:"user_id"`
	Nickname        string    `gorm:"type:varchar(100)" json:"nickname"`
	Content         string    `gorm:"type:text" json:"content"`
	OriginalContent string    `gorm:"type:text" json:"original_content,omitempty"` // 原始消息内容
	Forwards        string    `gorm:"type:text" json:"forwards,omitempty"`         // 合并转发内容的 JSON
}

func (MessageLog) TableName() string { return "message_logs" }

// Sticker 收集的表情包
type Sticker struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	FileName    string `gorm:"type:varchar(100)" json:"file_name"`            // 本地文件名（uuid.ext）
	FileHash    string `gorm:"type:varchar(64);uniqueIndex" json:"file_hash"` // 文件 MD5 哈希（用于去重）
	Description string `gorm:"type:text" json:"description"`                  // Vision 模型生成的描述
	UseCount    int    `gorm:"default:0" json:"use_count"`                    // 使用次数
}

func (Sticker) TableName() string { return "stickers" }

// MoodState 面向某个用户的情绪状态
type MoodState struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	UserID    int64     `gorm:"uniqueIndex" json:"user_id"`
	UpdatedAt time.Time `json:"updated_at"`

	// 情绪快变量
	Valence     float64 `gorm:"default:0.0" json:"valence"`     // [-1.0, 1.0] 心情好坏：负数=心情差，正数=心情好
	Energy      float64 `gorm:"default:0.5" json:"energy"`      // [0.0, 1.0] 精神/活跃度：低=疲惫，高=活跃
	Sociability float64 `gorm:"default:0.5" json:"sociability"` // [0.0, 1.0] 社交意愿：低=想安静，高=想聊天
	Irritation  float64 `gorm:"default:0.0" json:"irritation"`  // [0.0, 1.0] 烦躁度：高=更容易不耐烦
	Curiosity   float64 `gorm:"default:0.5" json:"curiosity"`   // [0.0, 1.0] 兴趣度：高=更想继续了解/追问

	// 最后变化原因（用于调试）
	LastReason string `gorm:"type:varchar(200)" json:"last_reason,omitempty"`
}

func (MoodState) TableName() string { return "mood_state" }
