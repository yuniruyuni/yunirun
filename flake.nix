{
  description = "yuniruyuni.net の VPS 上でコンテナ化したアプリを動かす小さなデプロイシステム";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAll = f: nixpkgs.lib.genAttrs systems (s: f nixpkgs.legacyPackages.${s});
    in
    {
      packages = forAll (pkgs: rec {
        yunirun = pkgs.callPackage ./nix/package.nix { };
        default = yunirun;
      });

      # NixOS 側はこのモジュールを import して services.yunirun を設定する。
      # yunirun 自体は NixOS に依存しないので、このモジュールは配線でしかない。
      nixosModules.default = import ./nix/module.nix self;

      # 実際の NixOS を VM で起動して確かめる e2e。
      #
      # 単体テストでは押さえられない部分 —— systemd・PostgreSQL・podman が
      # 揃った環境でしか起きない事柄 —— を検証する。これまで本番で踏んだ
      # 問題の多くはこの層にあった。
      #
      # KVM が要るので x86_64-linux だけに絞る。GitHub Actions のランナーには
      # /dev/kvm がある。
      checks.x86_64-linux = {
        converge = import ./nix/tests/converge.nix {
          pkgs = nixpkgs.legacyPackages.x86_64-linux;
          inherit self;
        };
      };

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; };
      });
    };
}
