package bot

import (
	"context"
	"strings"
	"testing"
)

func TestFuncPlugin(t *testing.T) {
	ctx := context.Background()
	var (
		calls  []string
		gotReg Registrar
	)
	p := &FuncPlugin{
		Meta: Metadata{Name: "inline", Version: "v0.1.0"},
		OnSetup: func(_ context.Context, reg Registrar) error {
			calls = append(calls, "setup")
			gotReg = reg
			return nil
		},
		OnStart: func(context.Context) error { calls = append(calls, "start"); return nil },
		OnStop:  func(context.Context) error { calls = append(calls, "stop"); return nil },
	}

	if got := p.Metadata().Name; got != "inline" {
		t.Fatalf("Metadata().Name = %q, want inline", got)
	}
	reg := NewRecordingRegistrar()
	if err := p.Setup(ctx, reg); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if gotReg != Registrar(reg) {
		t.Fatal("OnSetup 未收到传入的 Registrar")
	}
	if err := p.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := strings.Join(calls, ","); got != "setup,start,stop" {
		t.Fatalf("调用顺序 = %q, want setup,start,stop", got)
	}

	// nil 接收者与缺省字段都是空实现，不 panic。
	var nilPlugin *FuncPlugin
	if got := nilPlugin.Metadata(); got.Name != "" || len(got.Permissions) != 0 {
		t.Fatalf("nil Metadata = %+v, want 零值", got)
	}
	if err := nilPlugin.Setup(ctx, nil); err != nil {
		t.Fatalf("nil Setup: %v", err)
	}
	if err := nilPlugin.Start(ctx); err != nil {
		t.Fatalf("nil Start: %v", err)
	}
	if err := nilPlugin.Stop(ctx); err != nil {
		t.Fatalf("nil Stop: %v", err)
	}

	empty := &FuncPlugin{}
	if err := empty.Setup(ctx, nil); err != nil {
		t.Fatalf("缺 OnSetup: %v", err)
	}
	if err := empty.Start(ctx); err != nil {
		t.Fatalf("缺 OnStart: %v", err)
	}
	if err := empty.Stop(ctx); err != nil {
		t.Fatalf("缺 OnStop: %v", err)
	}
	if got := empty.Metadata().Name; got != "" {
		t.Fatalf("缺 Meta 的名字 = %q, want 空", got)
	}
}
