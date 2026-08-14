package store

import (
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// DefaultGroupName 首次启动创建的默认归属。
const DefaultGroupName = "default"

// Options 连接池设置。PG 每条连接是一个服务端进程，默认 max_connections=100，
// 所以客户端这边必须设上限——Go 的默认是「无上限」，压测时会直接把 PG 打满。
type Options struct {
	MaxOpen int
	MaxIdle int
}

// Open 连接 PostgreSQL、迁移表结构、写入默认配置/默认归属/初始管理员。
func Open(dsn, adminUser, adminPass string, opt Options) (*gorm.DB, error) {
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
		// 批量插入时不需要 GORM 回填自增主键（日志表主键是我们自己生成的 uuid），
		// 关掉能少一轮 RETURNING。
		SkipDefaultTransaction: true,
	})
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	if opt.MaxOpen <= 0 {
		opt.MaxOpen = 32
	}
	if opt.MaxIdle <= 0 || opt.MaxIdle > opt.MaxOpen {
		opt.MaxIdle = opt.MaxOpen / 4
	}
	sqlDB.SetMaxOpenConns(opt.MaxOpen)
	sqlDB.SetMaxIdleConns(opt.MaxIdle)
	sqlDB.SetConnMaxLifetime(time.Hour)
	if err := sqlDB.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	// 先把 groups 建好并写入默认归属，users.group_id 的默认值才有意义
	if err := db.AutoMigrate(&Group{}); err != nil {
		return nil, fmt.Errorf("automigrate groups: %w", err)
	}
	DB = db
	defaultGroup, err := seedDefaultGroup(db)
	if err != nil {
		return nil, err
	}

	// 顺序有讲究：外键约束是打开的（PG 加列不重建表，不像 sqlite），
	// 被引用的表必须先建出来。
	if err := db.AutoMigrate(
		&User{}, &Channel{}, &ChannelKey{}, &Model{}, &Binding{}, &APIKey{}, &RequestLog{}, &Setting{},
	); err != nil {
		return nil, fmt.Errorf("automigrate: %w", err)
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
