package bot

import (
	"context"
	"testing"
)

// nilAdapter 是只用于注册表测试的空实现。
type nilAdapter struct{}

func (nilAdapter) Name() string                                    { return "nil" }
func (nilAdapter) Start(ctx context.Context, sink EventSink) error { return nil }
func (nilAdapter) Stop(ctx context.Context) error                  { return nil }
func (nilAdapter) Send(ctx context.Context, req *SendRequest) (*SendResult, error) {
	return nil, nil
}
func (nilAdapter) Capabilities() Capabilities { return Capabilities{Text: true} }

func TestRegisterAdapterRejectsInvalid(t *testing.T) {
	beforeMeta := len(RegisteredAdapters())
	beforeRejects := len(AdapterRegistrationErrors())

	RegisterAdapter(AdapterMetadata{}, func(AdapterContext) (Adapter, error) { return nil, nil })
	RegisterAdapter(AdapterMetadata{Name: "adapter-with-nil-factory"}, nil)

	// 非法注册不入表，但必须被记录：否则只能在运行时表现为「未注册」。
	if got := len(RegisteredAdapters()); got != beforeMeta {
		t.Fatalf("非法注册不应入表: %d -> %d", beforeMeta, got)
	}
	rejects := AdapterRegistrationErrors()
	if len(rejects) != beforeRejects+2 {
		t.Fatalf("非法注册应被记录: %d -> %d", beforeRejects, len(rejects))
	}
	if got := rejects[beforeRejects]; got.Name != "" || got.Reason != "注册名为空" {
		t.Fatalf("空注册名的记录不符: %+v", got)
	}
	if got := rejects[beforeRejects+1]; got.Name != "adapter-with-nil-factory" || got.Reason != "工厂为 nil" {
		t.Fatalf("nil 工厂的记录不符: %+v", got)
	}
}

func TestLookupAdapterSnapshot(t *testing.T) {
	meta := AdapterMetadata{
		Name:        "registry-test",
		Version:     "v1",
		Platforms:   []string{"regplat"},
		Permissions: []Permission{PermNetwork},
		Options:     []string{OptListenAddr},
	}
	RegisterAdapter(meta, func(AdapterContext) (Adapter, error) { return nilAdapter{}, nil })

	// 入参切片在注册后被修改，不得影响注册表。
	meta.Platforms[0] = "mutated"
	meta.Permissions[0] = PermStorage

	got, factory, ok := LookupAdapter("registry-test")
	if !ok || factory == nil {
		t.Fatalf("LookupAdapter(registry-test) = %v, %v", factory, ok)
	}
	if got.Platforms[0] != "regplat" || got.Permissions[0] != PermNetwork || got.Options[0] != OptListenAddr {
		t.Fatalf("注册表元信息被入参污染: %+v", got)
	}

	// 出参切片被修改，同样不得影响后续查询。
	got.Platforms[0] = "another"
	again, _, _ := LookupAdapter("registry-test")
	if again.Platforms[0] != "regplat" {
		t.Fatalf("快照未隔离: %+v", again)
	}

	if _, _, ok := LookupAdapter("registry-test-missing"); ok {
		t.Fatal("未注册的适配器不应命中")
	}
}

func TestAdapterMetadataHasPermission(t *testing.T) {
	meta := AdapterMetadata{Permissions: []Permission{PermNetListen, PermNetwork}}
	if !meta.HasPermission(PermNetListen) || !meta.HasPermission(PermNetwork) {
		t.Fatal("已声明权限应命中")
	}
	if meta.HasPermission(PermStorage) {
		t.Fatal("未声明权限不应命中")
	}

	// 适配器不得通过 PermAll 获得权限。
	all := AdapterMetadata{Permissions: []Permission{PermAll}}
	if all.HasPermission(PermNetwork) {
		t.Fatal("PermAll 不适用于适配器")
	}
}
