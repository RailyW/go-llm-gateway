package store

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
)

// JSONB 对应 PostgreSQL 的 jsonb 列，是一段原始 JSON 字节。
//
// 为什么要有它：网关里有不少「结构不稳定、又不值得单独建列」的东西——
// 上游返回的原始 usage 对象（anthropic 有 cache_creation_input_tokens、
// openai responses 有 reasoning_tokens，各家还在加）、上游的扩展配置、
// 将来的重试轨迹等等。给每个字段建列会让表结构一直被动跟着上游变，
// 塞进一个 jsonb 列则既能存、又能查（`usage->>'reasoning_tokens'`）、还能建 GIN 索引。
//
// 注意：请求/响应**原文**不放数据库，仍然落磁盘归档文件。原文是增长最快的部分，
// 必须能独立冷备/迁到对象存储，不该跟着数据库一起膨胀。
//
// 用 []byte 而不是 map/any：读写都不需要反序列化，API 里可以原样透传。
//
// 一个 jsonb 的语义要记住：它存的是**解析后的结构**，不是原始文本——
// key 顺序、重复 key、空白都不保留。需要逐字节保真就得用 text/json 类型，
// 但那样就没法用 jsonb 运算符和 GIN 索引了；我们要的是可查，所以选 jsonb。
type JSONB []byte

// GormDataType 让 AutoMigrate 建出 jsonb 列（而不是 bytea）。
func (JSONB) GormDataType() string { return "jsonb" }

// Value 空值写 NULL，避免 jsonb 列里塞空字符串（那是非法 JSON）。
func (j JSONB) Value() (driver.Value, error) {
	if len(j) == 0 {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("jsonb: 非法 JSON: %.64s", j)
	}
	return string(j), nil
}

func (j *JSONB) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append((*j)[:0], v...)
	case string:
		*j = append((*j)[:0], v...)
	default:
		return errors.New("jsonb: 无法扫描的类型")
	}
	return nil
}

// MarshalJSON 直接透传，API 输出里就是原始 JSON 而不是 base64 字符串。
func (j JSONB) MarshalJSON() ([]byte, error) {
	if len(j) == 0 {
		return []byte("null"), nil
	}
	return j, nil
}

func (j *JSONB) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*j = nil
		return nil
	}
	*j = append((*j)[:0], b...)
	return nil
}
