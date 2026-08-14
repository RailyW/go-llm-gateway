package store

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// DefaultGroupName 首次启动创建的默认归属。
const DefaultGroupName = "default"

// Open 打开 sqlite、迁移表结构、写入默认配置/默认归属/初始管理员。
func Open(dbPath, adminUser, adminPass string) (*gorm.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	// _pragma 让并发下少些 database is locked。
	// 不开 foreign_keys：sqlite 加/删列是「重建表」，开了外键会让迁移直接失败。
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
		// sqlite 下 gorm 迁移会重建表，外键约束会让加列（如 users.group_id）失败；
		// 关联完整性由 handler 层保证（删归属前检查引用）。
		DisableForeignKeyConstraintWhenMigrating: true,
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	hadChannelAPIKey := db.Migrator().HasTable(&Channel{}) && db.Migrator().HasColumn(&Channel{}, "api_key")

	// 先把 groups 建好并写入默认归属，users.group_id 默认值才有意义
	if err := db.AutoMigrate(&Group{}); err != nil {
		return nil, fmt.Errorf("automigrate groups: %w", err)
	}
	DB = db
	defaultGroup, err := seedDefaultGroup(db)
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(
		&User{}, &Channel{}, &ChannelKey{}, &Model{}, &Binding{}, &APIKey{}, &RequestLog{}, &Setting{},
	); err != nil {
		return nil, fmt.Errorf("automigrate: %w", err)
	}
	// 老库里 users 没有 group_id / 或为 0，兜底落到默认归属
	if err := db.Model(&User{}).Where("group_id = 0 OR group_id IS NULL").
		Update("group_id", defaultGroup.ID).Error; err != nil {
		return nil, err
	}
	if hadChannelAPIKey {
		if err := migrateChannelAPIKeys(db, defaultGroup.ID); err != nil {
			return nil, err
		}
	}
	if err := seedSettings(db); err != nil {
		return nil, err
	}
	if err := seedAdmin(db, adminUser, adminPass, defaultGroup.ID); err != nil {
		return nil, err
	}
	return db, nil
}

func seedDefaultGroup(db *gorm.DB) (*Group, error) {
	var g Group
	err := db.Session(&gorm.Session{Logger: logger.Discard}).Where("name = ?", DefaultGroupName).First(&g).Error
	if err == nil {
		return &g, nil
	}
	if err != gorm.ErrRecordNotFound {
		return nil, err
	}
	g = Group{Name: DefaultGroupName, Remark: "默认归属", Enabled: true}
	if err := db.Create(&g).Error; err != nil {
		return nil, err
	}
	log.Printf("[init] 已创建默认归属 %q (id=%d)", g.Name, g.ID)
	return &g, nil
}

// migrateChannelAPIKeys 老结构里每个上游只有一把 key（channels.api_key），
// 迁移成默认归属下的一把 ChannelKey，然后删掉旧列。
func migrateChannelAPIKeys(db *gorm.DB, groupID uint) error {
	type row struct {
		ID     uint
		APIKey string
	}
	var rows []row
	if err := db.Table("channels").Select("id, api_key").Where("api_key != ''").Scan(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		var n int64
		if err := db.Model(&ChannelKey{}).Where("channel_id = ?", r.ID).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if err := db.Create(&ChannelKey{
			ChannelID: r.ID, GroupID: groupID, Name: "migrated", Key: r.APIKey, Weight: 1, Enabled: true,
		}).Error; err != nil {
			return err
		}
		log.Printf("[migrate] 上游 %d 的 api_key 已迁移为归属 %d 下的一把 key", r.ID, groupID)
	}
	return db.Migrator().DropColumn(&Channel{}, "api_key")
}

func seedAdmin(db *gorm.DB, username, password string, groupID uint) error {
	var n int64
	if err := db.Model(&User{}).Where("role = ?", RoleAdmin).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	u := &User{Username: username, PasswordHash: string(hash), Role: RoleAdmin, GroupID: groupID, Enabled: true}
	if err := db.Create(u).Error; err != nil {
		return err
	}
	log.Printf("[init] 已创建初始管理员 %s / %s ，请尽快修改密码", username, password)
	return nil
}
