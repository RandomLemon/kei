// Package sql 是 SQL 存储后端的显式 opt-in 入口。
//
// 只做空导入：注册基于 GORM 的 `sqlite` 与 `mysql` 方言，使装配期能解析
// `storage.type: sqlite/mysql`（见 internal/storage 的类型注册表）。这些方言分别
// 依赖 cgo 版 `github.com/mattn/go-sqlite3` 与 `github.com/go-sql-driver/mysql`，
// 会让二进制引入 GORM 与 cgo，因此不放在门面 pkg/kei 里自动注册。
//
// 使用方式：用 `storage.type: sqlite/mysql` 时在入口空导入本包：
//
//	import _ "github.com/RandomLemon/kei/pkg/kei/sql"
//
// 只用 `memory`（默认）或自行注入 `kei.Options.Storage` 时不要导入，以免白白拉入
// GORM 与 cgo 依赖。`cmd/bot` 已空导入本包，保持 CLI 对 sqlite/mysql 的既有支持。
package sql

import (
	_ "github.com/RandomLemon/kei/internal/storage/mysql"
	_ "github.com/RandomLemon/kei/internal/storage/sqlite"
)
