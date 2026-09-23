{
  description = "kei — 可插拔的 Go chatbot 框架（Engine / EventBus / Adapter / Plugin）";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = nixpkgs.lib.genAttrs systems;
      pkgsFor = system: nixpkgs.legacyPackages.${system};

      # devShell 与 packages 共用同一份工具链定义，避免两个环境漂移。
      devTools =
        pkgs: with pkgs; [
          go # 编译器 / go test / go vet
          gopls # LSP
          gotools # goimports / guru 等辅助工具
          golangci-lint # 静态检查（可选，见 README）
          delve # 调试器（dlv）
          protobuf # protoc：阶段 6 gRPC 插件使用
          protoc-gen-go
          protoc-gen-go-grpc
          buf # proto 校验 / lint
          jq # 手工调试 Mock/飞书回调的 JSON 载荷
          curl
          git
        ];
    in
    {
      devShells = forAllSystems (system: {
        default = (pkgsFor system).mkShell {
          name = "kei";

          packages = devTools (pkgsFor system);

          # 只使用 devShell 提供的 Go，禁止 go 自动下载其它 toolchain。
          env.GOTOOLCHAIN = "local";

          shellHook = ''
            echo "kei dev shell · $(go version)"
            echo "常用命令: go build ./... | go test -race ./... | go vet ./... | buf lint"
          '';
        };
      });

      packages = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
          kei = pkgs.buildGoModule {
            pname = "kei";
            version = "0.1.0";
            src = self;
            subPackages = [
              "cmd/bot"
              "cmd/example-plugin"
            ];
            # 依赖变化后运行 `nix build` 并按报错信息替换该哈希。
            vendorHash = "sha256-32RD9Os/Pr6w0Ql+G2AoTpg/vqArJkY7lSfLZG0Sy1M=";
            ldflags = [
              "-s"
              "-w"
            ];
            meta = {
              description = "可插拔的 Go chatbot 框架";
              mainProgram = "bot";
              license = pkgs.lib.licenses.mit;
            };
          };
        in
        {
          default = kei;
          inherit kei;
        }
      );

      apps = forAllSystems (system: {
        default = {
          type = "app";
          program = "${self.packages.${system}.kei}/bin/bot";
        };
        example-plugin = {
          type = "app";
          program = "${self.packages.${system}.kei}/bin/example-plugin";
        };
      });

      checks = forAllSystems (system: {
        default = self.packages.${system}.kei;
      });

      # nixfmt-tree 让 `nix fmt` 在无参数时也能格式化目录树下的所有 .nix 文件。
      formatter = forAllSystems (system: (pkgsFor system).nixfmt-tree);
    };
}
