# services.yunirun — yunirun を設置して設定するだけの配線。
#
# yunirun 自体は NixOS に依存しない。ここでやるのは
#   - バイナリの設置
#   - 取り込むリポジトリ一覧の書き出し
#   - activation のあとに converge を走らせる
#   - migration を root 側で実行するための unit と、それを起動するための
#     narrow な sudo ルール
#   - opkssh の認可 (取り込み一覧と一対一)
# だけ。アプリの中身に関する設定はここには来ない。
self:
{ config, lib, pkgs, ... }:

let
  cfg = config.services.yunirun;
  pkg = self.packages.${pkgs.stdenv.hostPlatform.system}.yunirun;

  configFile = pkgs.writeText "yunirun-config.json" (builtins.toJSON {
    inherit (cfg) domain adminRecipient hostKeyPath basePort baseUID;
    stateDir = cfg.stateDir;
    apps = cfg.apps;
  });

  # yunirun が実行時に呼ぶ外部コマンド。PATH をここで決めることで、
  # どの実装を使うかをシステム側が握れる。
  runtimePath = lib.makeBinPath [
    pkgs.podman
    pkgs.age
    pkgs.postgresql
    pkgs.curl
    pkgs.systemd
    pkgs.shadow
    # runuser。psql を postgres として実行するのに使う。
    pkgs.util-linux
    pkgs.coreutils
    pkgs.sudo
    "/run/wrappers"
  ];
in
{
  options.services.yunirun = {
    enable = lib.mkEnableOption "yunirun";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkg;
      description = "使用する yunirun。";
    };

    domain = lib.mkOption {
      type = lib.types.str;
      description = "各アプリのホスト名を導出する元。<app>.<domain> になる。";
    };

    stateDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/yunirun";
      description = "yunirun が持つ状態の置き場所。";
    };

    hostKeyPath = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/agenix/age-key.txt";
      description = ''
        生成した秘密の暗号化に使うホスト側の age 秘密鍵。
        既存の agenix と同じ鍵を使うので、鍵の管理箇所が増えない。
      '';
    };

    adminRecipient = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''
        生成した秘密を復号できる管理者の age 公開鍵。
        ホストを失ったときの復旧経路になるので設定を強く勧める。
      '';
    };

    basePort = lib.mkOption {
      type = lib.types.int;
      default = 0;
      description = ''
        ホストポート割り当ての起点。0 なら既定値 (8100)。
        既存の仕組みと並行して動かす間、帯を重ねないために使う。
      '';
    };

    baseUID = lib.mkOption {
      type = lib.types.int;
      default = 0;
      description = "uid/gid 割り当ての起点。0 なら既定値 (6000)。";
    };

    apps = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = { };
      example = { fighter = "yuniruyuni/fighter-notes"; };
      description = ''
        取り込むアプリ。名前からリポジトリ (owner/name) への対応。

        ここに書くのは取り込みの意思決定だけで、アプリの中身に関する設定は
        各リポジトリの yunirun.jsonc にある。この一覧がそのまま opkssh の
        認可リストになるので、アプリ側が自分を勝手に取り込ませることはできない。
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];
    environment.etc."yunirun/config.json".source = configFile;

    # activation のあとに収束させる。
    #
    # activation の後でなければならないのは、NixOS が /etc/subuid を宣言から
    # 再生成して yunirun が足した行を消すため。ここで復元する。
    systemd.services.yunirun-converge = {
      description = "yunirun: 宣言されたアプリ一覧に実体を一致させる";
      wantedBy = [ "multi-user.target" ];
      after = [ "postgresql.service" "network-online.target" ];
      wants = [ "postgresql.service" ];
      restartTriggers = [ configFile ];
      path = [ cfg.package ];
      environment.PATH = lib.mkForce runtimePath;
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${cfg.package}/bin/yunirun converge";
      };
    };

    # schema の適用。deploy ユーザはこれを起動できるが、owner パスワードを
    # 読むことはできない。
    systemd.services."yunirun-migrate@" = {
      description = "yunirun: %i の schema を適用する";
      environment.PATH = lib.mkForce runtimePath;
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${cfg.package}/bin/yunirun migrate %i";
      };
    };

    # deploy ユーザに許すのは自分のアプリの migration を起動することだけ。
    security.sudo.extraRules = lib.mapAttrsToList (name: _: {
      users = [ "yunirun-${name}" ];
      commands = [{
        command = "${pkgs.systemd}/bin/systemctl start yunirun-migrate@${name}.service";
        options = [ "NOPASSWD" ];
      }];
    }) cfg.apps;

    # 取り込み一覧がそのまま認可になる。
    services.opkssh.authorizations = lib.mapAttrsToList (name: repo: {
      user = "yunirun-${name}";
      principal = "repo:${repo}:ref:refs/heads/main";
      issuer = "https://token.actions.githubusercontent.com";
    }) cfg.apps;

    # HAProxy は yunirun が生成した設定を読む。アプリの増減に追従させるため、
    # NixOS の services.haproxy (設定が eval 時に固定される) は使わない。
    systemd.services.yunirun-haproxy = {
      description = "yunirun: HAProxy";
      wantedBy = [ "multi-user.target" ];
      after = [ "yunirun-converge.service" ];
      requires = [ "yunirun-converge.service" ];
      serviceConfig = {
        Type = "notify";
        ExecStart = "${pkgs.haproxy}/bin/haproxy -Ws -f /etc/yunirun/haproxy.cfg";
        ExecReload = "${pkgs.haproxy}/bin/haproxy -Ws -f /etc/yunirun/haproxy.cfg -c -q";
        Restart = "always";
      };
    };

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 root root -"
      "d /run/yunirun 0755 root root -"
    ];
  };
}
