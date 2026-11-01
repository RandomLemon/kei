package storage

import (
	"context"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// denied 是拒绝一切访问的 bot.Storage 实现。
//
// 全部方法返回 bot.ErrPermissionDenied（哨兵定义在公开 SDK，插件可经
// errors.Is 判定）。
type denied struct{}

// 确保 denied 满足 bot.Storage。
var _ bot.Storage = denied{}

// 确保 denied 同时满足 bot.TableStorage。
var _ bot.TableStorage = denied{}

// Denied 返回一个始终拒绝访问的 Storage，用于未声明 PermStorage 的插件。
func Denied() bot.Storage { return denied{} }

// DeniedTables 返回一个始终拒绝访问的 TableStorage，用于未声明 PermStorage 的
// 插件/适配器；与 Denied 是同一实现的表视图。
func DeniedTables() bot.TableStorage { return denied{} }

func (denied) Get(context.Context, string) ([]byte, error) {
	return nil, bot.ErrPermissionDenied
}

func (denied) Set(context.Context, string, []byte, time.Duration) error {
	return bot.ErrPermissionDenied
}

func (denied) Delete(context.Context, string) error {
	return bot.ErrPermissionDenied
}

func (denied) CreateTable(context.Context, string) error { return bot.ErrPermissionDenied }

func (denied) ListTables(context.Context) ([]string, error) { return nil, bot.ErrPermissionDenied }

func (denied) RenameTable(context.Context, string, string) error { return bot.ErrPermissionDenied }

func (denied) DropTable(context.Context, string) error { return bot.ErrPermissionDenied }

func (denied) InsertRow(context.Context, string, string, []byte) error {
	return bot.ErrPermissionDenied
}

func (denied) GetRow(context.Context, string, string) ([]byte, error) {
	return nil, bot.ErrPermissionDenied
}

func (denied) UpdateRow(context.Context, string, string, []byte) error {
	return bot.ErrPermissionDenied
}

func (denied) DeleteRow(context.Context, string, string) error { return bot.ErrPermissionDenied }

func (denied) ListRows(context.Context, string) ([]bot.TableRow, error) {
	return nil, bot.ErrPermissionDenied
}
