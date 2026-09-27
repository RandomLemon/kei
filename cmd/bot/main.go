// Command bot 是 kei 框架的入口程序。
//
// 职责只剩命令行参数、信号与空导入：装配逻辑全部在 pkg/kei 门面里
// （加载配置 -> 装配适配器与插件 -> 启动引擎 -> 优雅退出），因此，需要在
// 代码里嵌入 chatbot 的使用方可以 import pkg/kei 而无需复制本文件。
//
// 所有平台细节都在 adapters/ 或第三方适配器包内，本文件不含任何平台名分支。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/RandomLemon/kei/pkg/bot"
	kei "github.com/RandomLemon/kei/pkg/kei"

	// 内置适配器与插件通过空导入注册到各自的注册表，是否启用由配置决定。
	// 第三方适配器（独立包或独立 module）以同样方式接入，无需改动本文件以外的代码。
	_ "github.com/RandomLemon/kei/adapters/feishu"
	_ "github.com/RandomLemon/kei/adapters/mock"
	_ "github.com/RandomLemon/kei/adapters/onebot"
	_ "github.com/RandomLemon/kei/plugins/echo"
	_ "github.com/RandomLemon/kei/plugins/manage"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bot:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("bot", flag.ContinueOnError)
	configPath := fs.String("config", "configs/config.yaml", "配置文件路径")
	showVersion := fs.Bool("version", false, "打印版本并退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *showVersion {
		fmt.Printf("kei v%s\n", bot.Version)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return kei.Run(ctx, kei.Options{ConfigFile: *configPath})
}
