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

// Denied 返回一个始终拒绝访问的 Storage，用于未声明 PermStorage 的插件。
func Denied() bot.Storage { return denied{} }

func (denied) Get(context.Context, string) ([]byte, error) {
	return nil, bot.ErrPermissionDenied
}

func (denied) Set(context.Context, string, []byte, time.Duration) error {
	return bot.ErrPermissionDenied
}

func (denied) Delete(context.Context, string) error {
	return bot.ErrPermissionDenied
}
