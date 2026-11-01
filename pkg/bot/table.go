package bot

import (
	"context"
	"errors"
)

// 表级与表内 CRUD 的错误哨兵。实现必须保证对应用户可观察行为返回可被
// errors.Is 识别的错误。
var (
	// ErrTableNotFound 表示 Storage 中不存在所请求的表。
	ErrTableNotFound = errors.New("bot: storage: table not found")
	// ErrTableExists 表示要创建或重命名为的表已存在。
	ErrTableExists = errors.New("bot: storage: table already exists")
	// ErrRowExists 表示表内已存在同键的行。
	ErrRowExists = errors.New("bot: storage: row already exists")
)

// TableRow 是表内的一行：键与值。值由实现拷贝，修改返回值不影响存储。
type TableRow struct {
	// Key 是行键，非空且不超过 191 字节。
	Key string
	// Value 是行值，可为空（nil 视同空值）。
	Value []byte
}

// TableStorage 是 Storage 之上的可选表级与表内 CRUD 能力。
//
// 与 Storage 的 KV 模型并存而不是替换它：表是命名集合，行是无模式的
// 「非空字符串键 + 字节串值」，不提供列 schema、TTL、分页、事务或跨表操作。
// 内置 memory/sqlite/mysql 后端由同一个根实例同时实现 Storage 与 TableStorage。
//
// 表必须显式经 CreateTable 创建；InsertRow 不会自动建表。表名与行键均按字节
// 原样存储，不做大小写归一，上限 191 字节（插件/适配器视图的表名前缀也计入）。
// 行键只在表内唯一：不同表可以有相同行键。实现必须并发安全，且所有方法必须
// 尊重 ctx。
type TableStorage interface {
	// CreateTable 创建表；表已存在时返回 ErrTableExists。
	CreateTable(ctx context.Context, table string) error
	// ListTables 按名称升序列出全部表；无表时返回空切片。
	ListTables(ctx context.Context) ([]string, error)
	// RenameTable 把 oldName 重命名为 newName 并保留表内数据。
	//
	// oldName 不存在时返回 ErrTableNotFound；newName 已存在时返回 ErrTableExists；
	// oldName == newName 时源表存在返回 nil，不存在返回 ErrTableNotFound。
	RenameTable(ctx context.Context, oldName, newName string) error
	// DropTable 删除表及其全部行；表不存在时返回 nil。
	DropTable(ctx context.Context, table string) error

	// InsertRow 在表中插入一行；表不存在时返回 ErrTableNotFound，
	// 行键已存在时返回 ErrRowExists。入参被拷贝。
	InsertRow(ctx context.Context, table, key string, value []byte) error
	// GetRow 读取一行；表不存在时返回 ErrTableNotFound，
	// 行不存在时返回 ErrNotFound。返回值是副本。
	GetRow(ctx context.Context, table, key string) ([]byte, error)
	// UpdateRow 覆盖一行；表不存在时返回 ErrTableNotFound，
	// 行不存在时返回 ErrNotFound。入参被拷贝。
	UpdateRow(ctx context.Context, table, key string, value []byte) error
	// DeleteRow 删除一行；表不存在时返回 ErrTableNotFound，
	// 行不存在时返回 nil。
	DeleteRow(ctx context.Context, table, key string) error
	// ListRows 按行键升序返回表内全部行；表不存在时返回 ErrTableNotFound，
	// 空表返回空切片。返回值是副本。
	ListRows(ctx context.Context, table string) ([]TableRow, error)
}
