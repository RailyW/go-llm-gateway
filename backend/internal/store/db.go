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

// Open 打开 sqlite、迁移表结构、写入默认配置与初始管理员。
func Open(dbPath, adminUser, adminPass string) (*gorm.DB, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	// _pragma 让并发下少些 database is locked
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.AutoMigrate(&User{}, &Channel{}, &Model{}, &Binding{}, &APIKey{}, &RequestLog{}, &Setting{}); err != nil {
		return nil, fmt.Errorf("automigrate: %w", err)
	}
	DB = db

	if err := seedSettings(db); err != nil {
		return nil, err
	}
	if err := seedAdmin(db, adminUser, adminPass); err != nil {
		return nil, err
	}
	return db, nil
}

func seedAdmin(db *gorm.DB, username, password string) error {
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
	u := &User{Username: username, PasswordHash: string(hash), Role: RoleAdmin, Enabled: true}
	if err := db.Create(u).Error; err != nil {
		return err
	}
	log.Printf("[init] 已创建初始管理员 %s / %s ，请尽快修改密码", username, password)
	return nil
}
