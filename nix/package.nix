{
  lib,
  buildGoModule,
  go_1_26,
  fetchurl,
  versionCheckHook,
}:

let
  # go.mod's toolchain directive does not select the compiler in a Nix build.
  # GO-2026-6809 requires the patched crypto/x509 trust store in the binary.
  securityGo = if lib.versionAtLeast go_1_26.version "1.26.10" then go_1_26 else
    go_1_26.overrideAttrs (_: {
      version = "1.26.10";
      src = fetchurl {
        url = "https://go.dev/dl/go1.26.10.src.tar.gz";
        hash = "sha256-/WPog7ovK/ypPBZGsx6+9j4rHk0rMXyXidl2po2B828=";
      };
    });
in
(buildGoModule.override { go = securityGo; }) (finalAttrs: {
  pname = "yunirun";
  version = "0.1.0";

  src = lib.cleanSource ../.;

  vendorHash = "sha256-0u3k/I6RVw0iCcu3rzQfN5MbFsmWVmrzFkMad+QJkVA=";

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
