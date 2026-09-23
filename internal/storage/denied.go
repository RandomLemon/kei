package storage

import (
	"context"
	"errors"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

// ErrPermissionDenied 表示插件未声明 storage 权限而被拒绝访问。
var ErrPermissionDenied = errors.New("storage: permission denied")

// denied 是拒绝一切访问的 bot.Storage 实现。
type denied struct{}

// 确保 denied 满足 bot.Storage。
var _ bot.Storage = denied{}

// Denied 返回一个始终拒绝访问的 Storage，用于未声明 PermStorage 的插件。
func Denied() bot.Storage { return denied{} }

func (denied) Get(context.Context, string) ([]byte, error) {
	return nil, ErrPermissionDenied
}

func (denied) Set(context.Context, string, []byte, time.Duration) error {
	return ErrPermissionDenied
}

func (denied) Delete(context.Context, string) error {
	return ErrPermissionDenied
}
