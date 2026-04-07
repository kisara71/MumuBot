package memory

import (
	"mumu-bot/internal/session"
	"time"
)

type ConversationRef = session.Ref

func AllConversationRef() ConversationRef {
	return session.AllConversationRef()
}

func GroupConversationRef(groupID int64) ConversationRef {
	return session.GroupConversationRef(groupID)
}

func PrivateConversationRef(userID int64) ConversationRef {
	return session.PrivateConversationRef(userID)
}

// MemoryType 记忆类型
type MemoryType string

const (
	MemoryTypeGroupFact      MemoryType = "group_fact"      // 群长期事实（群规、群风格、重要事件等）
	MemoryTypeUserFact       MemoryType = "user_fact"       // 某个用户的稳定事实，尤其适合私聊沉淀
	MemoryTypeSelfExperience MemoryType = "self_experience" // 自身经历（参与的事、被提及、感受等）
	MemoryTypeConversation   MemoryType = "conversation"    // 对话中的重要信息或阶段性上下文
)

// Memory 长期记忆
// TODO
type Memory struct {
	ID             uint       `gorm:"primarykey" json:"id"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	ConversationID string     `gorm:"type:varchar(32);index" json:"conversation_id"`
	Type           MemoryType `gorm:"type:varchar(50);index" json:"type"`
	GroupID        int64      `gorm:"index" json:"group_id"`
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

	UserID      int64     `gorm:"uniqueIndex::idx_user" json:"user_id"`
	Nickname    string    `gorm:"type:varchar(100)" json:"nickname"`
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

func (UserProfile) TableName() string { return "member_profiles" }

type StyleIntent string

const (
	StyleIntentLightBanter StyleIntent = "轻松起哄"
	StyleIntentAgreement   StyleIntent = "认同接话"
	StyleIntentQuestioning StyleIntent = "询问推进"
	StyleIntentCalming     StyleIntent = "安抚缓和"
)

var styleIntentSet = map[StyleIntent]struct{}{
	StyleIntentLightBanter: {},
	StyleIntentAgreement:   {},
	StyleIntentQuestioning: {},
	StyleIntentCalming:     {},
}

func IsValidStyleIntent(v string) bool {
	_, ok := styleIntentSet[StyleIntent(v)]
	return ok
}

func StyleIntentValues() []string {
	return []string{
		string(StyleIntentLightBanter),
		string(StyleIntentAgreement),
		string(StyleIntentQuestioning),
		string(StyleIntentCalming),
	}
}

type StyleTone string

const (
	StyleToneDirect     StyleTone = "直接"
	StyleToneLight      StyleTone = "轻松"
	StyleToneExaggerate StyleTone = "夸张"
	StyleToneRestrained StyleTone = "克制"
)

var styleToneSet = map[StyleTone]struct{}{
	StyleToneDirect:     {},
	StyleToneLight:      {},
	StyleToneExaggerate: {},
	StyleToneRestrained: {},
}

func IsValidStyleTone(v string) bool {
	_, ok := styleToneSet[StyleTone(v)]
	return ok
}

func StyleToneValues() []string {
	return []string{
		string(StyleToneDirect),
		string(StyleToneLight),
		string(StyleToneExaggerate),
		string(StyleToneRestrained),
	}
}

type StyleCardStatus string

const (
	StyleCardStatusCandidate StyleCardStatus = "candidate"
	StyleCardStatusActive    StyleCardStatus = "active"
	StyleCardStatusRejected  StyleCardStatus = "rejected"
)

// StyleCard 群风格卡片
type StyleCard struct {
	ID             uint            `gorm:"primarykey" json:"id"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	ConversationID string          `gorm:"type:varchar(32);index" json:"conversation_id"`
	GroupID        int64           `gorm:"index" json:"group_id"`
	Intent         string          `gorm:"type:varchar(32);index" json:"intent"`
	Tone           string          `gorm:"type:varchar(32);index" json:"tone"`
	TriggerRule    string          `gorm:"type:varchar(255)" json:"trigger_rule"`
	AvoidRule      string          `gorm:"type:varchar(255)" json:"avoid_rule"`
	Example        string          `gorm:"type:varchar(255)" json:"example"`
	SourceExcerpt  string          `gorm:"type:text" json:"source_excerpt"`
	Status         StyleCardStatus `gorm:"type:varchar(20);index;default:'candidate'" json:"status"`
	EvidenceCount  int             `gorm:"default:1" json:"evidence_count"`
	UseCount       int             `gorm:"default:0" json:"use_count"`
	LastUsedAt     *time.Time      `json:"last_used_at,omitempty"`
}

var styleCardTableName = "style_cards"

func (StyleCard) TableName() string { return styleCardTableName }

// Jargon 黑话/术语
type Jargon struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	GroupID  int64  `gorm:"index" json:"group_id"`
	UserID   int64  `gorm:"index" json:"user_id"`
	Content  string `gorm:"type:varchar(100);index" json:"content"`
	Meaning  string `gorm:"type:text" json:"meaning"`
	Context  string `gorm:"type:text" json:"context"`
	Checked  bool   `gorm:"default:false" json:"checked"`
	Rejected bool   `gorm:"default:false" json:"rejected"`
}

func (Jargon) TableName() string { return "jargons" }

// MessageLog 消息日志
type MessageLog struct {
	ID              uint      `gorm:"primarykey" json:"id"`
	CreatedAt       time.Time `gorm:"index" json:"created_at"`
	MessageID       string    `gorm:"type:varchar(100);uniqueIndex" json:"message_id"`
	ConversationID  string    `gorm:"type:varchar(32);index" json:"conversation_id"`
	GroupID         int64     `gorm:"index" json:"group_id"`
	UserID          int64     `gorm:"index" json:"user_id"`
	Nickname        string    `gorm:"type:varchar(100)" json:"nickname"`
	Content         string    `gorm:"type:text" json:"content"`
	OriginalContent string    `gorm:"type:text" json:"original_content,omitempty"` // 原始消息内容
	MessageSource   string    `gorm:"type:varchar(50);index" json:"message_source"`
	IsMentioned     bool      `gorm:"default:false" json:"is_mentioned"`
	Forwards        string    `gorm:"type:text" json:"forwards,omitempty"` // 合并转发内容的 JSON
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

// LearningState 学习状态记录
type LearningState struct {
	ID        uint      `gorm:"primarykey" json:"id"`
	UpdatedAt time.Time `json:"updated_at"`

	GroupID       int64 `gorm:"uniqueIndex" json:"group_id"`
	LastMessageID uint  `json:"last_message_id"` // 上次学习到的最后一条消息ID (数据库自增ID)
}

func (LearningState) TableName() string { return "learning_states" }
