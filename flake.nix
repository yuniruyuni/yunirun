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

      devShells = forAll (pkgs: {
        default = pkgs.mkShell { packages = [ pkgs.go pkgs.gopls ]; };
      });
    };
}
