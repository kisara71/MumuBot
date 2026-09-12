package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

var (
	cfg     *Config
	loadErr error
	once    sync.Once
)

type Config struct {
	App       AppConfig       `yaml:"app"`
	Persona   PersonaConfig   `yaml:"persona"`
	OneBot    OneBotConfig    `yaml:"onebot"`
	Users     []UserConfig    `yaml:"users"`
	Agent     AgentConfig     `yaml:"agent"`
	Chat      ChatConfig      `yaml:"chat"`
	LLM       LLMConfig       `yaml:"llm"`
	Embedding EmbeddingConfig `yaml:"embedding"`
	VisionLLM VisionLLMConfig `yaml:"vision_llm"`
	Memory    MemoryConfig    `yaml:"memory"`
	Sticker   StickerConfig   `yaml:"sticker"`
	Server    ServerConfig    `yaml:"server"`
	Debug     DebugConfig     `yaml:"debug"`
}

type AppConfig struct {
	Debug    bool   `yaml:"debug"`
	LogLevel string `yaml:"log_level"`
}

type PersonaConfig struct {
	Name          string   `yaml:"name"`
	Interests     []string `yaml:"interests"`
	SpeakingStyle string   `yaml:"speaking_style"`
	Personality   string   `yaml:"personality"`
}

type OneBotConfig struct {
	WsURL             string `yaml:"ws_url"`
	AccessToken       string `yaml:"access_token"`
	ReconnectInterval int    `yaml:"reconnect_interval"`
}

type UserConfig struct {
	UserID      int64  `yaml:"user_id"`
	Enabled     bool   `yaml:"enabled"`
	ExtraPrompt string `yaml:"extra_prompt"`
}

type AgentConfig struct {
	ThinkDebounceMS        int  `yaml:"think_debounce_ms"`
	MessageBufferSize      int  `yaml:"message_buffer_size"`
	MaxStep                int  `yaml:"max_step"`
	MaxCoroutine           int  `yaml:"max_coroutine"`
	EnableActiveRetrieval  bool `yaml:"enable_active_retrieval"`
	ProactiveCheckInterval int  `yaml:"proactive_check_interval_sec"`
}

type ChatConfig struct {
	TypingSimulation bool            `yaml:"typing_simulation"`
	TypingSpeed      int             `yaml:"typing_speed"`
	Proactive        ProactiveConfig `yaml:"proactive"`
}

// ProactiveConfig creates one future contact time after a conversation.
// It intentionally has no per-tick probability: repeated Bernoulli trials caused
// the old burst/silence behaviour.
type ProactiveConfig struct {
	Enabled        bool `yaml:"enabled"`
	MinIdleMinutes int  `yaml:"min_idle_minutes"`
	MaxIdleMinutes int  `yaml:"max_idle_minutes"`
	QuietStartHour int  `yaml:"quiet_start_hour"`
	QuietEndHour   int  `yaml:"quiet_end_hour"`
}

type LLMConfig struct {
	APIKey      string                 `yaml:"api_key"`
	BaseURL     string                 `yaml:"base_url"`
	Model       string                 `yaml:"model"`
	ExtraFields map[string]interface{} `yaml:"extra_fields"`
}

type EmbeddingConfig struct {
	Enabled bool   `yaml:"enabled"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

type VisionLLMConfig struct {
	Enabled bool   `yaml:"enabled"`
	APIKey  string `yaml:"api_key"`
	BaseURL string `yaml:"base_url"`
	Model   string `yaml:"model"`
}

type MemoryConfig struct {
	MySQL             MySQLConfig             `yaml:"mysql"`
	Milvus            MilvusConfig            `yaml:"milvus"`
	MessageLogCleanup MessageLogCleanupConfig `yaml:"message_log_cleanup"`
}

type MessageLogCleanupConfig struct {
	Enabled       *bool `yaml:"enabled"`
	IntervalHours int   `yaml:"interval_hours"`
	KeepLatest    int   `yaml:"keep_latest"`
}

type MySQLConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	DBName   string `yaml:"db_name"`
}

type MilvusConfig struct {
	Address        string `yaml:"address"`
	DBName         string `yaml:"db_name"`
	CollectionName string `yaml:"collection_name"`
	VectorDim      int    `yaml:"vector_dim"`
	MetricType     string `yaml:"metric_type"`
}

type StickerConfig struct {
	AutoSave    bool   `yaml:"auto_save"`
	StoragePath string `yaml:"storage_path"`
	MaxSizeMB   int    `yaml:"max_size_mb"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type DebugConfig struct {
	ShowPrompt    bool `yaml:"show_prompt"`
	ShowThinking  bool `yaml:"show_thinking"`
	ShowMemory    bool `yaml:"show_memory"`
	ShowToolCalls bool `yaml:"show_tool_calls"`
}

func Load(path string) (*Config, error) {
	once.Do(func() {
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			loadErr = readErr
			return
		}

		cfg = &Config{}
		if loadErr = yaml.Unmarshal(data, cfg); loadErr != nil {
			cfg = nil
			return
		}
		applyEnvironment(cfg)
		loadErr = validate(cfg)
		if loadErr != nil {
			cfg = nil
		}
	})
	return cfg, loadErr
}

func applyEnvironment(c *Config) {
	if value := os.Getenv("LUMA_LLM_API_KEY"); value != "" {
		c.LLM.APIKey = value
	}
	if value := os.Getenv("LUMA_EMBEDDING_API_KEY"); value != "" {
		c.Embedding.APIKey = value
	} else if c.Embedding.APIKey == "" {
		c.Embedding.APIKey = c.LLM.APIKey
	}
	if value := os.Getenv("LUMA_VISION_API_KEY"); value != "" {
		c.VisionLLM.APIKey = value
	} else if c.VisionLLM.APIKey == "" {
		c.VisionLLM.APIKey = c.LLM.APIKey
	}
	if value := os.Getenv("LUMA_ONEBOT_TOKEN"); value != "" {
		c.OneBot.AccessToken = value
	}
	if value := os.Getenv("LUMA_MYSQL_PASSWORD"); value != "" {
		c.Memory.MySQL.Password = value
	}
}

func validate(c *Config) error {
	if c.Persona.Name == "" {
		return fmt.Errorf("persona.name 必须配置")
	}
	if c.OneBot.WsURL == "" {
		return fmt.Errorf("onebot.ws_url 必须配置")
	}
	oneBotURL, err := url.Parse(c.OneBot.WsURL)
	if err != nil || (oneBotURL.Scheme != "ws" && oneBotURL.Scheme != "wss") || oneBotURL.Host == "" {
		return fmt.Errorf("onebot.ws_url 必须是有效的 ws 或 wss 地址")
	}
	if c.OneBot.ReconnectInterval <= 0 {
		c.OneBot.ReconnectInterval = 5
	}
	if c.LLM.Model == "" || c.LLM.BaseURL == "" {
		return fmt.Errorf("llm.model 和 llm.base_url 必须配置")
	}
	if c.Embedding.Enabled {
		if c.Embedding.Model == "" || c.Embedding.BaseURL == "" {
			return fmt.Errorf("启用 embedding 时 model 和 base_url 必须配置")
		}
		if c.Memory.Milvus.Address == "" {
			c.Memory.Milvus.Address = "127.0.0.1:19530"
		}
		if c.Memory.Milvus.DBName == "" {
			c.Memory.Milvus.DBName = "default"
		}
		if c.Memory.Milvus.CollectionName == "" {
			c.Memory.Milvus.CollectionName = "luma_memories"
		}
		if c.Memory.Milvus.VectorDim <= 0 {
			c.Memory.Milvus.VectorDim = 1024
		}
		if c.Memory.Milvus.MetricType == "" {
			c.Memory.Milvus.MetricType = "COSINE"
		}
		switch strings.ToUpper(c.Memory.Milvus.MetricType) {
		case "COSINE", "IP", "L2":
			c.Memory.Milvus.MetricType = strings.ToUpper(c.Memory.Milvus.MetricType)
		default:
			return fmt.Errorf("memory.milvus.metric_type 只支持 COSINE、IP 或 L2")
		}
	}
	if c.VisionLLM.Enabled && (c.VisionLLM.Model == "" || c.VisionLLM.BaseURL == "") {
		return fmt.Errorf("启用 vision_llm 时 model 和 base_url 必须配置")
	}
	if c.Memory.MySQL.Host == "" {
		c.Memory.MySQL.Host = "127.0.0.1"
	}
	if c.Memory.MySQL.Port <= 0 {
		c.Memory.MySQL.Port = 3306
	}
	if c.Memory.MySQL.User == "" {
		return fmt.Errorf("memory.mysql.user 必须配置")
	}
	if c.Memory.MySQL.DBName == "" {
		c.Memory.MySQL.DBName = "luma"
	}
	if len(c.Users) == 0 {
		return fmt.Errorf("至少需要配置一个私聊用户")
	}
	enabledUsers := 0
	seenUsers := make(map[int64]struct{}, len(c.Users))
	for _, user := range c.Users {
		if user.UserID <= 0 {
			return fmt.Errorf("users.user_id 必须是正整数")
		}
		if _, exists := seenUsers[user.UserID]; exists {
			return fmt.Errorf("users 中存在重复用户: %d", user.UserID)
		}
		seenUsers[user.UserID] = struct{}{}
		if user.Enabled {
			enabledUsers++
		}
	}
	if enabledUsers == 0 {
		return fmt.Errorf("至少需要启用一个私聊用户")
	}
	if c.Chat.Proactive.QuietStartHour < 0 || c.Chat.Proactive.QuietStartHour > 23 ||
		c.Chat.Proactive.QuietEndHour < 0 || c.Chat.Proactive.QuietEndHour > 23 {
		return fmt.Errorf("主动联系静默时段必须在 0 到 23 点之间")
	}
	if c.Chat.Proactive.MinIdleMinutes <= 0 {
		c.Chat.Proactive.MinIdleMinutes = 90
	}
	if c.Chat.Proactive.MaxIdleMinutes < c.Chat.Proactive.MinIdleMinutes {
		c.Chat.Proactive.MaxIdleMinutes = c.Chat.Proactive.MinIdleMinutes
	}
	if c.Agent.ProactiveCheckInterval <= 0 {
		c.Agent.ProactiveCheckInterval = 60
	}
	if c.Agent.ThinkDebounceMS <= 0 {
		c.Agent.ThinkDebounceMS = 3200
	}
	if c.Agent.MessageBufferSize <= 0 {
		c.Agent.MessageBufferSize = 40
	}
	if c.Agent.MaxStep <= 0 {
		c.Agent.MaxStep = 6
	}
	if c.Agent.MaxCoroutine <= 0 {
		c.Agent.MaxCoroutine = 3
	}
	if c.Chat.TypingSpeed <= 0 {
		c.Chat.TypingSpeed = 6
	}
	if c.Sticker.StoragePath == "" {
		c.Sticker.StoragePath = "./stickers"
	}
	if c.Sticker.MaxSizeMB <= 0 {
		c.Sticker.MaxSizeMB = 2
	}
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port <= 0 {
		c.Server.Port = 8080
	}
	if c.Server.Port > 65535 {
		return fmt.Errorf("server.port 必须在 1 到 65535 之间")
	}
	return nil
}

func Get() *Config { return cfg }

func (c *Config) GetUserConfig(userID int64) *UserConfig {
	for i := range c.Users {
		if c.Users[i].UserID == userID {
			return &c.Users[i]
		}
	}
	return nil
}

func (c *Config) IsUserEnabled(userID int64) bool {
	user := c.GetUserConfig(userID)
	return user != nil && user.Enabled
}
