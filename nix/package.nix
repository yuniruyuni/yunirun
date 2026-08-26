{
  lib,
  buildGoModule,
  versionCheckHook,
}:

buildGoModule (finalAttrs: {
  pname = "yunirun";
  version = "0.1.0";

  src = lib.cleanSource ../.;

  vendorHash = "sha256-holdL/SYwJfKdF+07w1fBUARfzmPGA3PNQtqFAtEWDs=";

  ldflags = [
    "-s"
    "-w"
    "-X=main.version=${finalAttrs.version}"
  ];

  # 実行時に呼ぶ外部コマンド (podman / psql / systemctl / shadow) は
  # 意図的に PATH 解決に任せている。NixOS モジュール側で systemd の path を
  # 与える方が、どの実装を使うかをシステム側で決められて都合が良い。

  nativeInstallCheckInputs = [ versionCheckHook ];
  doInstallCheck = true;
  versionCheckProgramArg = "--version";

  meta = {
    description = "yuniruyuni.net の VPS 上でコンテナ化したアプリを動かす小さなデプロイシステム";
    homepage = "https://github.com/yuniruyuni/yunirun";
    license = lib.licenses.mit;
    mainProgram = "yunirun";
  };
})
