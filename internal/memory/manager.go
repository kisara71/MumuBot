package memory

import (
	"context"
	"errors"
	"fmt"
	"github.com/kisara71/luma/internal/config"
	"github.com/kisara71/luma/internal/session"
	"github.com/kisara71/luma/internal/utils"
	"github.com/kisara71/luma/internal/vector"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// EmbeddingProvider 向量嵌入接口
type EmbeddingProvider interface {
	Embed(ctx context.Context, text string) ([]float64, error)
}

type vectorStore interface {
	Insert(ctx context.Context, memoryID uint, refID string, memType string, embedding []float64) (int64, error)
	Search(ctx context.Context, embedding []float64, refID string, memType string, topK int, threshold float64) ([]vector.SearchResult, error)
	Delete(ctx context.Context, memoryIDs []uint) error
	DeleteByRef(ctx context.Context, refID string) error
	Close() error
	GetConfig() *vector.MilvusConfig
}

// Manager 记忆系统管理器
type Manager struct {
	db          *gorm.DB
	embedding   EmbeddingProvider
	milvus      vectorStore // Memory 向量存储
	cleanupStop chan struct{}
	background  sync.WaitGroup
	closeOnce   sync.Once
	closeErr    error
}

func buildLikeQuery(columns []string, keywords []string) (string, []interface{}) {
	if len(columns) == 0 || len(keywords) == 0 {
		return "", nil
	}
	likeConditions := make([]string, 0, len(keywords))
	args := make([]interface{}, 0, len(columns)*len(keywords))
	for _, kw := range keywords {
		columnConds := make([]string, 0, len(columns))
		pattern := "%" + kw + "%"
		for _, column := range columns {
			columnConds = append(columnConds, column+" LIKE ?")
			args = append(args, pattern)
		}
		likeConditions = append(likeConditions, "("+strings.Join(columnConds, " OR ")+")")
	}
	return strings.Join(likeConditions, " OR "), args
}

func reverseMessageLogs(items []MessageLog) {
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
}

// NewManager 创建记忆管理器
func NewManager(embedding EmbeddingProvider) (*Manager, error) {
	// 构建 MySQL DSN
	cfg := config.Get()
	mysqlCfg := cfg.Memory.MySQL
	if mysqlCfg.Host == "" {
		mysqlCfg.Host = "127.0.0.1"
	}
	if mysqlCfg.Port == 0 {
		mysqlCfg.Port = 3306
	}
	if mysqlCfg.DBName == "" {
		mysqlCfg.DBName = "luma"
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		mysqlCfg.User,
		mysqlCfg.Password,
		mysqlCfg.Host,
		mysqlCfg.Port,
		mysqlCfg.DBName,
	)

	db, err := gorm.Open(mysql.Open(dsn))
	if err != nil {
		return nil, fmt.Errorf("连接 MySQL 数据库失败: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("获取 MySQL 连接失败: %w", err)
	}

	// 迁移所有表
	if err := db.AutoMigrate(
		&Memory{},
		&UserProfile{},
		&MessageLog{},
		&Sticker{},
		&MoodState{},
	); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	var milvusClient vectorStore
	if embedding != nil {
		milvusCfg := &vector.MilvusConfig{
			Address:        cfg.Memory.Milvus.Address,
			DBName:         cfg.Memory.Milvus.DBName,
			CollectionName: cfg.Memory.Milvus.CollectionName,
			VectorDim:      cfg.Memory.Milvus.VectorDim,
			MetricType:     cfg.Memory.Milvus.MetricType,
		}
		client, err := vector.NewMilvusClient(milvusCfg)
		if err != nil {
			_ = sqlDB.Close()
			return nil, fmt.Errorf("连接记忆 Milvus 失败: %w", err)
		}
		milvusClient = client
		zap.L().Info("Milvus 向量存储已连接", zap.String("collection", milvusCfg.CollectionName))
	} else {
		zap.L().Info("Embedding 未启用，长期记忆使用 MySQL 关键词检索")
	}

	m := &Manager{
		db:          db,
		embedding:   embedding,
		milvus:      milvusClient,
		cleanupStop: make(chan struct{}),
	}

	// 启动消息日志清理任务
	m.startMessageLogCleanup()

	// 启动情绪衰减任务
	m.startMoodDecay()

	return m, nil
}

// ==================== 短期记忆 ====================

// AddMessage 添加消息到短期记忆
func (m *Manager) AddMessage(msg MessageLog) error {
	return m.db.Create(&msg).Error
}

// GetRecentMessages 获取最近的消息记录
func (m *Manager) GetRecentMessages(ref session.Ref, limit, offset int) []MessageLog {
	var dbMsgs []MessageLog
	q := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).Order("created_at DESC").Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	q.Find(&dbMsgs)

	reverseMessageLogs(dbMsgs)
	return dbMsgs
}

// IsFirstConversation 判断当前关系是否处于首次对话阶段。
// 由于 onMessage 会先落库再进入 think，这里以当前会话消息总数 <= 1 视为首次。
func (m *Manager) IsFirstConversation(ref session.Ref) (bool, error) {
	if !ref.Valid() {
		return false, nil
	}

	var count int64
	err := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).Count(&count).Error
	if err != nil {
		return false, err
	}
	return count <= 1, nil
}

// ==================== 长期记忆 ====================

// SearchSimilarMemoriesByConversation 按会话引用和记忆类型搜索相似记忆
func (m *Manager) SearchSimilarMemoriesByConversation(ctx context.Context, text string, ref session.Ref, memType MemoryType, limit int, threshold float64) ([]Memory, error) {
	if m.milvus == nil || m.embedding == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 15
	}

	emb, err := m.embedding.Embed(ctx, text)
	if err != nil {
		return nil, err
	}

	results, err := m.milvusVectorSearch(ctx, emb, ref.ID(), normalizeMemoryTypeFilter(memType), limit, threshold)
	if err != nil {
		return nil, err
	}

	return results, nil
}

// UpdateMemoryContent 更新记忆内容（用于合并）
func (m *Manager) UpdateMemoryContent(ctx context.Context, id uint, newContent string) error {
	// 更新数据库
	if err := m.db.Model(&Memory{}).Where("id = ?", id).Update("content", newContent).Error; err != nil {
		return err
	}

	// 更新向量（先删后增）
	if m.milvus != nil && m.embedding != nil {
		_ = m.milvus.Delete(ctx, []uint{id})

		emb, err := m.embedding.Embed(ctx, newContent)
		if err == nil {
			var mem Memory
			if err := m.db.First(&mem, id).Error; err == nil {
				_, _ = m.milvus.Insert(ctx, id, mem.ConversationID, string(mem.Type), emb)
			}
		}
	}
	return nil
}

// DeleteMemory 删除记忆
func (m *Manager) DeleteMemory(ctx context.Context, id uint) error {
	if err := m.db.Delete(&Memory{}, id).Error; err != nil {
		return err
	}
	if m.milvus != nil {
		_ = m.milvus.Delete(ctx, []uint{id})
	}
	return nil
}

// SaveMemory 保存长期记忆
func (m *Manager) SaveMemory(ctx context.Context, mem *Memory) error {
	// 生成 embedding
	var embedding []float64
	if m.embedding != nil {
		if emb, err := m.embedding.Embed(ctx, mem.Content); err == nil {
			embedding = emb
		}
	}

	// 保存到 MySQL
	if err := m.db.Save(mem).Error; err != nil {
		return err
	}

	// 保存向量到 Milvus
	if m.milvus != nil && len(embedding) > 0 {
		if _, err := m.milvus.Insert(ctx, mem.ID, mem.ConversationID, string(mem.Type), embedding); err != nil {
			// 向量插入失败只记录日志，不影响主流程
			zap.L().Error("Milvus 插入向量失败", zap.Error(err))
		}
	}

	return nil
}

// QueryMemoryByConversation 查询相关记忆
func (m *Manager) QueryMemoryByConversation(ctx context.Context, query string, ref session.Ref, memType MemoryType, limit int) ([]Memory, error) {
	// 尝试 Milvus 向量搜索
	if m.milvus != nil && m.embedding != nil {
		if emb, err := m.embedding.Embed(ctx, query); err == nil {
			if results, err := m.milvusVectorSearch(ctx, emb, ref.ID(), normalizeMemoryTypeFilter(memType), limit, 0.7); err == nil && len(results) > 0 {
				return results, nil
			}
		}
	}

	// 回退到关键词搜索
	var memories []Memory
	q := scopeSession(ref, m.db.Model(&Memory{}))
	if memType != "" {
		q = q.Where("type = ?", memType)
	}
	keywords := strings.Fields(query)
	if len(keywords) == 0 {
		return memories, nil
	}
	condition, args := buildLikeQuery([]string{"content"}, keywords)
	err := q.Where(condition, args...).
		Order("importance DESC, updated_at DESC").
		Limit(limit).
		Find(&memories).Error
	if err != nil {
		return memories, err
	}

	if len(memories) > 0 {
		memoryIDs := make([]uint, 0, len(memories))
		for _, mem := range memories {
			memoryIDs = append(memoryIDs, mem.ID)
		}
		_ = m.db.Model(&Memory{}).Where("id IN ?", memoryIDs).Updates(map[string]any{
			"access_count": gorm.Expr("access_count + 1"),
		}).Error
	}

	return memories, nil
}

// startMessageLogCleanup 启动消息日志清理定时任务
func (m *Manager) startMessageLogCleanup() {
	cleanupCfg := config.Get().Memory.MessageLogCleanup
	enabled := true
	if cleanupCfg.Enabled != nil {
		enabled = *cleanupCfg.Enabled
	}
	if !enabled {
		return
	}

	intervalHours := cleanupCfg.IntervalHours
	if intervalHours <= 0 {
		intervalHours = 6
	}
	keepLatest := cleanupCfg.KeepLatest
	if keepLatest <= 0 {
		keepLatest = 500
	}

	// 启动后立即清理一次
	m.background.Add(1)
	go func() {
		defer m.background.Done()
		m.cleanupMessageLogs(keepLatest)
	}()

	ticker := time.NewTicker(time.Duration(intervalHours) * time.Hour)
	m.background.Add(1)
	go func() {
		defer m.background.Done()
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				m.cleanupMessageLogs(keepLatest)
			case <-m.cleanupStop:
				return
			}
		}
	}()
}

// cleanupMessageLogs 清理消息日志，仅保留每个会话最新的 keepLatest 条
func (m *Manager) cleanupMessageLogs(keepLatest int) {
	if keepLatest <= 0 {
		return
	}

	type logScope struct {
		ConversationID string
	}

	var scopes []logScope
	if err := m.db.Model(&MessageLog{}).
		Where("conversation_id <> ''").
		Select("conversation_id").
		Group("conversation_id").
		Scan(&scopes).Error; err != nil {
		zap.L().Warn("清理消息日志失败：获取会话列表失败", zap.Error(err))
		return
	}

	for _, scope := range scopes {
		ref, ok := session.ParseRefID(scope.ConversationID)
		if !ok {
			continue
		}

		var keepIDs []uint
		if err := scopeMessageLogs(ref, m.db.Model(&MessageLog{})).
			Order("created_at DESC").
			Limit(keepLatest).
			Pluck("id", &keepIDs).Error; err != nil {
			zap.L().Warn("清理消息日志失败：获取保留ID失败", zap.Int64("user_id", ref.UserID), zap.Error(err))
			continue
		}
		if len(keepIDs) == 0 {
			continue
		}

		result := scopeMessageLogs(ref, m.db.Where("id NOT IN ?", keepIDs)).Delete(&MessageLog{})
		if result.Error != nil {
			zap.L().Warn("清理消息日志失败：删除旧记录失败", zap.Int64("user_id", ref.UserID), zap.Error(result.Error))
			continue
		}
		if result.RowsAffected > 0 {
			zap.L().Info("消息日志已清理", zap.Int64("user_id", ref.UserID), zap.Int("deleted", int(result.RowsAffected)))
		}
	}
}

// milvusVectorSearch 使用 Milvus 进行向量搜索并返回完整的 Memory 对象
func (m *Manager) milvusVectorSearch(ctx context.Context, queryEmb []float64, refID string, memType string, limit int, threshold float64) ([]Memory, error) {
	results, err := m.milvus.Search(ctx, queryEmb, refID, memType, limit, threshold)
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, nil
	}

	// 获取对应的记忆
	memoryIDs := make([]uint, 0, len(results))
	for _, r := range results {
		memoryIDs = append(memoryIDs, r.MemoryID)
	}

	var memories []Memory
	if err := m.db.Where("id IN ?", memoryIDs).Find(&memories).Error; err != nil {
		return nil, err
	}

	// 按照搜索结果的顺序排序
	memoryMap := make(map[uint]Memory)
	for _, mem := range memories {
		memoryMap[mem.ID] = mem
	}

	sortedMemories := make([]Memory, 0, len(results))
	for _, r := range results {
		if mem, ok := memoryMap[r.MemoryID]; ok {
			m.db.Model(&mem).Updates(map[string]any{
				"access_count": gorm.Expr("access_count + 1"),
			})
			sortedMemories = append(sortedMemories, mem)
		}
	}

	return sortedMemories, nil
}

func normalizeMemoryTypeFilter(memType MemoryType) string {
	return strings.TrimSpace(string(memType))
}

// ==================== 用户画像 ====================

// GetUserProfile 获取用户画像
func (m *Manager) GetUserProfile(userID int64) (*UserProfile, error) {
	var profile UserProfile
	err := m.db.Where("user_id = ?", userID).First(&profile).Error
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

// GetOrCreateUserProfile 获取或创建用户画像
func (m *Manager) GetOrCreateUserProfile(userID int64, nickname string) (*UserProfile, error) {
	var profile UserProfile
	err := m.db.Where("user_id = ?", userID).First(&profile).Error

	if errors.Is(err, gorm.ErrRecordNotFound) {
		profile = UserProfile{
			UserID:      userID,
			Nickname:    nickname,
			Activity:    0.5, // 初始活跃度
			Intimacy:    0.3, // 初始亲密度
			Trust:       0.4,
			Familiarity: 0.2,
			Respect:     0.5,
			LastSpeak:   time.Now(),
		}
		if err := m.db.Create(&profile).Error; err != nil {
			return nil, err
		}
		return &profile, nil
	}
	return &profile, err
}

// UpdateUserProfile 更新用户画像
func (m *Manager) UpdateUserProfile(profile *UserProfile) error {
	return m.db.Save(profile).Error
}

// RecordUserMessage 原子记录一次用户发言，避免拆句并发到达时覆盖计数。
func (m *Manager) RecordUserMessage(userID int64, nickname string, at time.Time) error {
	if userID <= 0 {
		return fmt.Errorf("userID 必须是正整数")
	}
	if at.IsZero() {
		at = time.Now()
	}
	profile := UserProfile{
		UserID:      userID,
		Nickname:    nickname,
		Activity:    0.5,
		Intimacy:    0.3,
		Trust:       0.4,
		Familiarity: 0.2,
		Respect:     0.5,
		LastSpeak:   at,
		MsgCount:    1,
	}
	return m.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}},
		DoUpdates: clause.Assignments(map[string]any{
			"nickname":   nickname,
			"last_speak": at,
			"msg_count":  gorm.Expr("msg_count + 1"),
			"activity":   gorm.Expr("LEAST(activity + 0.05, 1.0)"),
		}),
	}).Create(&profile).Error
}

// ==================== 统计 ====================

// GetStats 获取统计信息
func (m *Manager) GetStats() map[string]int64 {
	stats := make(map[string]int64)
	var memories, users, messages int64
	m.db.Model(&Memory{}).Count(&memories)
	m.db.Model(&UserProfile{}).Count(&users)
	m.db.Model(&MessageLog{}).Count(&messages)
	stats["memories"] = memories
	stats["users"] = users
	stats["messages"] = messages
	return stats
}

// ==================== 列表查询（供管理界面用）====================

func (m *Manager) ListMemoriesByConversation(ref session.Ref, memType string, page, pageSize int) ([]Memory, int64, error) {
	var items []Memory
	var total int64

	q := scopeSession(ref, m.db.Model(&Memory{}))
	if memType != "" {
		q = q.Where("type = ?", memType)
	}
	q.Count(&total)

	err := q.Order("updated_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	return items, total, err
}

func (m *Manager) ListUserProfiles(page, pageSize int) ([]UserProfile, int64, error) {
	var items []UserProfile
	var total int64

	q := m.db.Model(&UserProfile{})
	q.Count(&total)

	err := q.Order("msg_count DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	return items, total, err
}

func (m *Manager) ListMessageLogsByConversation(ref session.Ref, page, pageSize int) ([]MessageLog, int64, error) {
	var items []MessageLog
	var total int64

	q := scopeMessageLogs(ref, m.db.Model(&MessageLog{}))
	q.Count(&total)

	err := q.Order("created_at DESC").Offset((page - 1) * pageSize).Limit(pageSize).Find(&items).Error
	return items, total, err
}

// GetMessageLogByID 根据消息ID获取消息日志
func (m *Manager) GetMessageLogByID(messageID string) (*MessageLog, error) {
	var log MessageLog
	err := m.db.Where("message_id = ?", messageID).First(&log).Error
	if err != nil {
		return nil, err
	}
	return &log, nil
}

// Close 关闭连接
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		close(m.cleanupStop)
		m.background.Wait()

		if m.milvus != nil {
			if err := m.milvus.Close(); err != nil {
				m.closeErr = err
			}
		}
		if sqlDB, err := m.db.DB(); err != nil {
			if m.closeErr == nil {
				m.closeErr = err
			}
		} else if err := sqlDB.Close(); err != nil && m.closeErr == nil {
			m.closeErr = err
		}
	})
	return m.closeErr
}

func (m *Manager) GetDB() *gorm.DB { return m.db }

// ==================== 表情包管理 ====================

// SaveSticker 保存表情包（通过哈希去重）
func (m *Manager) SaveSticker(sticker *Sticker) (bool, error) {
	// 先检查哈希是否已存在
	var existing Sticker
	err := m.db.Where("file_hash = ?", sticker.FileHash).First(&existing).Error
	if err == nil {
		// 已存在，返回重复标记
		return true, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return false, err
	}

	// 不存在，创建新记录
	if err := m.db.Create(sticker).Error; err != nil {
		return false, err
	}
	return false, nil
}

// GetStickerByID 根据ID获取表情包
func (m *Manager) GetStickerByID(id uint) (*Sticker, error) {
	var sticker Sticker
	err := m.db.First(&sticker, id).Error
	if err != nil {
		return nil, err
	}
	return &sticker, nil
}

// SearchStickers 搜索表情包
func (m *Manager) SearchStickers(keyword string, limit int) ([]Sticker, error) {
	var stickers []Sticker
	q := m.db.Model(&Sticker{})
	if keyword != "" {
		keywords := strings.Fields(keyword)
		likeConditions := make([]string, 0, len(keywords))
		args := make([]interface{}, 0, len(keywords))
		for _, kw := range keywords {
			likeConditions = append(likeConditions, "description LIKE ?")
			args = append(args, "%"+kw+"%")
		}
		q = q.Where(strings.Join(likeConditions, " OR "), args...)
	}
	err := q.Order("use_count DESC, updated_at DESC").Limit(limit).Find(&stickers).Error
	return stickers, err
}

// UpdateStickerUsage 更新表情包使用记录
func (m *Manager) UpdateStickerUsage(id uint) error {
	return m.db.Model(&Sticker{}).Where("id = ?", id).Updates(map[string]any{
		"use_count": gorm.Expr("use_count + 1"),
	}).Error
}

// GetStickerByHash 通过哈希获取表情包
func (m *Manager) GetStickerByHash(hash string) (*Sticker, error) {
	var sticker Sticker
	err := m.db.Where("file_hash = ?", hash).First(&sticker).Error
	if err != nil {
		return nil, err
	}
	return &sticker, nil
}

// ==================== 情绪状态管理 ====================

// startMoodDecay 启动情绪衰减定时任务（每分钟执行一次）
func (m *Manager) startMoodDecay() {
	ticker := time.NewTicker(1 * time.Minute)
	m.background.Add(1)
	go func() {
		defer m.background.Done()
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := m.ApplyMoodDecay(); err != nil {
					zap.L().Error("情绪衰减失败", zap.Error(err))
				}
			case <-m.cleanupStop:
				return
			}
		}
	}()
	zap.L().Info("情绪衰减任务已启动")
}

// GetMoodState 获取某个用户的情绪状态
func (m *Manager) GetMoodState(userID int64) (*MoodState, error) {
	if userID == 0 {
		return nil, fmt.Errorf("userID 不能为空")
	}
	var mood MoodState
	err := m.db.Where("user_id = ?", userID).First(&mood).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		mood = MoodState{
			UserID:      userID,
			Valence:     0.0,
			Energy:      0.5,
			Sociability: 0.5,
			Irritation:  0.0,
			Curiosity:   0.5,
		}
		if err := m.db.Create(&mood).Error; err != nil {
			return nil, err
		}
		return &mood, nil
	}
	if err != nil {
		return nil, err
	}
	return &mood, nil
}

// UpdateMoodState 更新某个用户的情绪状态（增量更新）
func (m *Manager) UpdateMoodState(userID int64, valenceDelta, energyDelta, sociabilityDelta, irritationDelta, curiosityDelta float64, reason string) (*MoodState, error) {
	mood, err := m.GetMoodState(userID)
	if err != nil {
		return nil, err
	}

	mood.Valence = utils.ClampFloat64(mood.Valence+valenceDelta, -1.0, 1.0)
	mood.Energy = utils.ClampFloat64(mood.Energy+energyDelta, 0.0, 1.0)
	mood.Sociability = utils.ClampFloat64(mood.Sociability+sociabilityDelta, 0.0, 1.0)
	mood.Irritation = utils.ClampFloat64(mood.Irritation+irritationDelta, 0.0, 1.0)
	mood.Curiosity = utils.ClampFloat64(mood.Curiosity+curiosityDelta, 0.0, 1.0)
	mood.LastReason = reason

	if err := m.db.Save(mood).Error; err != nil {
		return nil, err
	}
	return mood, nil
}

// ApplyMoodDecay 应用情绪自然衰减
func (m *Manager) ApplyMoodDecay() error {
	var moods []MoodState
	if err := m.db.Find(&moods).Error; err != nil {
		return err
	}
	for i := range moods {
		moods[i].Valence *= 0.95
		moods[i].Energy += (0.5 - moods[i].Energy) * 0.05
		moods[i].Sociability += (0.5 - moods[i].Sociability) * 0.05
		moods[i].Irritation += (0.0 - moods[i].Irritation) * 0.08
		moods[i].Curiosity += (0.5 - moods[i].Curiosity) * 0.05
		if err := m.db.Save(&moods[i]).Error; err != nil {
			return err
		}
	}
	return nil
}
