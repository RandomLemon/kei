// Package storage 提供 bot.Storage 的内存实现。
//
// 用于 MVP 与测试；数据仅存活于进程内，重启即丢失。
package storage

import (
	"context"
	"sync"
	"time"

	"github.com/RandomLemon/kei/pkg/bot"
)

const janitorInterval = time.Minute

// Memory 是 bot.Storage 的内存实现，并发安全。
type Memory struct {
	mu    sync.RWMutex
	items map[string]item
	now   func() time.Time

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

type item struct {
	value   []byte
	expires time.Time // 零值表示永不过期
}

// 确保 Memory 满足 bot.Storage。
var _ bot.Storage = (*Memory)(nil)

// NewMemory 构造内存存储并启动过期清理 goroutine。
//
// 使用完必须调用 Close 释放后台 goroutine。
func NewMemory() *Memory {
	m := &Memory{
		items: make(map[string]item),
		now:   time.Now,
		stop:  make(chan struct{}),
	}
	m.wg.Add(1)
	go m.janitor()
	return m
}

// Get 读取键值，键不存在或已过期时返回 bot.ErrNotFound。
func (m *Memory) Get(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	it, ok := m.items[key]
	m.mu.RUnlock()
	if !ok || m.expired(it, m.now()) {
		return nil, bot.ErrNotFound
	}
	return append([]byte(nil), it.value...), nil
}

// Set 写入键值，ttl <= 0 表示永不过期。
func (m *Memory) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	it := item{value: append([]byte(nil), value...)}
	if ttl > 0 {
		it.expires = m.now().Add(ttl)
	}
	m.mu.Lock()
	m.items[key] = it
	m.mu.Unlock()
	return nil
}

// Delete 删除键值，键不存在时不返回错误。
func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.items, key)
	m.mu.Unlock()
	return nil
}

// Len 返回当前条目数（含未清理的过期条目）。
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.items)
}

// Close 停止后台清理 goroutine，可重复调用。
func (m *Memory) Close() error {
	m.stopOnce.Do(func() { close(m.stop) })
	m.wg.Wait()
	return nil
}

func (m *Memory) janitor() {
	defer m.wg.Done()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.sweep()
		}
	}
}

func (m *Memory) sweep() {
	now := m.now()
	m.mu.Lock()
	for k, it := range m.items {
		if m.expired(it, now) {
			delete(m.items, k)
		}
	}
	m.mu.Unlock()
}

func (m *Memory) expired(it item, now time.Time) bool {
	return !it.expires.IsZero() && !it.expires.After(now)
}
