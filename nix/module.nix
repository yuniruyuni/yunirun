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

  # apps の値は「リポジトリ名だけの文字列」と「attrset」の両方を許す。
  # 大半のアプリは前者で足りるので、ここで後者へ揃えてから使う。
  appDefs = lib.mapAttrs
    (_: v: if lib.isString v then { repo = v; principal = null; } else v)
    cfg.apps;

  # opkssh に渡す identity。GitHub OIDC の sub と完全一致する必要がある。
  #
  # 既定は素の repo:<owner>/<repo>:ref:refs/heads/main。ただしリポジトリが
  # sub claim をカスタマイズしていると前半が変わり (数値 id を含む形など)、
  # job に environment: が付くと後半も environment:<name> に変わる。
  # 導出では追いつかないので、その場合は principal を明示する。
  principalOf = a:
    if a.principal != null then a.principal
    else "repo:${a.repo}:ref:refs/heads/main";

  configFile = pkgs.writeText "yunirun-config.json" (builtins.toJSON {
    inherit (cfg) domain adminRecipient hostKeyPath basePort baseUID;
    stateDir = cfg.stateDir;
    homesDir = cfg.homesDir;
    # yunirun 本体が要るのは名前とリポジトリの対応だけ。認可は NixOS 側の
    # 仕事なので、principal は渡さない。
    apps = lib.mapAttrs (_: a: a.repo) appDefs;
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

    homesDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/yunirun-apps";
      description = ''
        アプリのホームを置く場所。stateDir の外に置く。

        stateDir は台帳と秘密のために root 専用 (0700) にしてあり、パスの途中が
        辿れないと配下のホームへもアプリのユーザから届かないため。
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
      type = lib.types.attrsOf (lib.types.either lib.types.str
        (lib.types.submodule {
          options = {
            repo = lib.mkOption {
              type = lib.types.str;
              description = "リポジトリ (owner/name)。";
            };
            principal = lib.mkOption {
              type = lib.types.nullOr lib.types.str;
              default = null;
              description = ''
                opkssh に渡す identity。GitHub OIDC の sub と完全一致させる。

                null なら repo:<owner>/<repo>:ref:refs/heads/main を使う。
                リポジトリが sub claim をカスタマイズしている場合や、job に
                environment: が付く場合は形が変わるので、実測した値をここに書く。
                実測方法:
                  gh api repos/<owner>/<repo>/actions/oidc/customization/sub
              '';
            };
          };
        }));
      default = { };
      example = {
        post = "yuniruyuni/StreamerPost";
        fighter = {
          repo = "yuniruyuni/FighterNotes";
          principal = "repo:yuniruyuni@85034901/FighterNotes@1313852776:ref:refs/heads/main";
        };
      };
      description = ''
        取り込むアプリ。名前からリポジトリへの対応。

        値は文字列 (リポジトリ名だけ) か attrset。attrset にすると opkssh の
        認可先 principal を明示できる。

        ここに書くのは取り込みの意思決定だけで、アプリの中身に関する設定は
        各リポジトリの yunirun.jsonc にある。この一覧がそのまま opkssh の
        認可リストになるので、アプリ側が自分を勝手に取り込ませることはできない。
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # yunirun を手で実行できるようにする。
    #
    # 実行時に呼ぶ外部コマンドも一緒に入れる。systemd 経由なら PATH を与えて
    # いるが、管理者が手で converge を打つ場合はシステムの PATH しか無い。
    # 実際 VM テストで age-keygen が見つからず落ちた。
    environment.systemPackages = [ cfg.package pkgs.age pkgs.podman ];
    environment.etc."yunirun/config.json".source = configFile;

    # activation のあとに収束させる。
    #
    # activation の後でなければならないのは、NixOS が /etc/subuid を宣言から
    # 再生成して yunirun が足した行を消すため。ここで復元する。
    systemd.services.yunirun-converge = {
      description = "yunirun: 宣言されたアプリ一覧に実体を一致させる";
      wantedBy = [ "multi-user.target" ];
      after = [ "postgresql.service" "network-online.target" ];
      # network-online は after だけでなく wants も要る。順序だけ指定しても
      # target 自体が起動しないので、依存として宣言していないと警告になる。
      wants = [ "postgresql.service" "network-online.target" ];
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
    #
    # パスは /run/current-system/sw/bin を使う。sudo はコマンドを文字列で
    # 照合するので、${pkgs.systemd}/bin/systemctl と書くと、呼び出し側が PATH で
    # 解決した /run/current-system/sw/bin/systemctl と一致せず拒否される
    # (実体は同じでも文字列が違う)。
    security.sudo.extraRules = lib.mapAttrsToList (name: _: {
      users = [ "yunirun-${name}" ];
      commands = [
        {
          command = "/run/current-system/sw/bin/systemctl start yunirun-migrate@${name}.service";
          options = [ "NOPASSWD" ];
        }
        # 受け取ったマニフェストを反映させるために converge を動かす。
        # unit を書くのは converge の仕事なので、これが無いと宣言が変わっても
        # 反映されない。converge は宣言に無いことは何もしないので、これを
        # 許しても他アプリへ影響を与えることはできない。
        #
        # start ではなく restart を許す。converge は RemainAfterExit=yes で
        # active (exited) のまま留まるため、start では何も起きない。
        {
          command = "/run/current-system/sw/bin/systemctl restart yunirun-converge.service";
          options = [ "NOPASSWD" ];
        }
      ];
    }) appDefs;

    # 取り込み一覧がそのまま認可になる。
    services.opkssh.authorizations = lib.mapAttrsToList (name: a: {
      user = "yunirun-${name}";
      principal = principalOf a;
      issuer = "https://token.actions.githubusercontent.com";
    }) appDefs;

    # HAProxy は yunirun が生成した設定を読む。アプリの増減に追従させるため、
    # NixOS の services.haproxy (設定が eval 時に固定される) は使わない。
    systemd.services.yunirun-haproxy = {
      description = "yunirun: HAProxy";
      wantedBy = [ "multi-user.target" ];
      # converge の後に始めるが、失敗しても道連れにしない。
      #
      # Requires にしていたところ、1 アプリの収束に失敗しただけで HAProxy まで
      # 停止し、収束できた他のアプリが巻き添えで落ちた。converge は失敗した
      # アプリをスキップして残りを収束させ、経路も保つ作りにしてあるので、
      # 設定は書き出されている。
      after = [ "yunirun-converge.service" ];
      wants = [ "yunirun-converge.service" ];
      serviceConfig = {
        Type = "notify";
        ExecStart = "${pkgs.haproxy}/bin/haproxy -Ws -f /etc/yunirun/haproxy.cfg";
        # reload は 2 段。まず設定を検査し (壊れた設定で reload すると
        # マスターが古い設定のまま残り、何が起きたか分かりにくい)、通れば
        # SIGUSR2 でマスターに再読込を指示する。
        #
        # -c だけを書いていたときは検査しかせず、新しいアプリを足しても
        # listen が増えなかった。設定には backend があるのにポートが開かない
        # という分かりにくい状態になる。
        ExecReload = [
          "${pkgs.haproxy}/bin/haproxy -Ws -f /etc/yunirun/haproxy.cfg -c -q"
          "${pkgs.coreutils}/bin/kill -USR2 $MAINPID"
        ];
        Restart = "always";
      };
    };

    systemd.tmpfiles.rules = [
      # stateDir には台帳と秘密が入るので root 専用。
      "d ${cfg.stateDir} 0700 root root -"
      # ホームは stateDir の外。各アプリのホーム自体は 0700 でユーザ所有になる。
      "d ${cfg.homesDir} 0755 root root -"
      "d /run/yunirun 0755 root root -"
    ];
  };
}
