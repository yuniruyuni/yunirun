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
    inherit (cfg) domain adminRecipient hostKeyPath secretsKeyPath basePort baseUID;
    haproxyImage = cfg.haproxyImage;
    observability = cfg.observability;
    stateDir = cfg.stateDir;
    homesDir = cfg.homesDir;
    dbDir = cfg.dbDir;
    envDir = cfg.envDir;
    dbImage = cfg.dbImage;
    # yunirun 本体が要るのは名前とリポジトリの対応だけ。認可は NixOS 側の
    # 仕事なので、principal は渡さない。
    apps = lib.mapAttrs (_: a: a.repo) cfg.apps;
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

    secretsKeyPath = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''
        アプリ側の秘密 (secrets/<ENV_NAME>.age) を復号する age 秘密鍵。

        hostKeyPath とは分ける。あちらは ssh のホスト鍵から導いているので
        ホストを作り直すと変わるが、アプリのリポジトリにある暗号文は人が
        暗号化したもので、鍵が変わると全アプリで暗号化し直しになる。

        対応する公開鍵は yunirun recipient で表示できる。
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

    dbDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/yunirun-db";
      description = ''
        アプリ専用 PostgreSQL のデータとソケットを置く場所。

        ホームとは分ける。ホームは rename や remove が捨てるので、そこに
        データを置くと名前を変えただけで消える。

        podman の名前付きボリュームを使わないのも同じ理由で、
        podman system prune --volumes の射程に入れないため。
      '';
    };

    envDir = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/yunirun-env";
      description = ''
        unit が EnvironmentFile= で読む env ファイルを置く場所。

        tmpfs ではなく永続領域に置く。/run に置いていたころ、再起動で消えた
        env を unit が参照して起動に失敗していた。converge は同じ target に
        居るだけで順序関係が無く、しかもアプリ側はユーザ unit なので
        システム unit を After= できない。順序を張るのではなく依存を消す。

        stateDir の中には置けない。stateDir は root 専用 (0700) だが、
        アプリのユーザ unit が自分の runtime.env まで辿れる必要がある。
      '';
    };

    observability = lib.mkOption {
      default = { };
      description = ''
        計測基盤 (メトリクス・ログ・可視化) を立てるかどうか。

        yunirun が管理する付帯コンテナとして立てる。取り込み対象は HAProxy の
        exporter で、そこに全アプリの応答が集まっているため、アプリ側に計装を
        足さずに済む。ログは journald を読むので、こちらもアプリ側の変更が要らない。

        どれも 127.0.0.1 にだけ bind する。外から見るときは ssh のポート転送を使う。
      '';
      type = lib.types.submodule {
        options = {
          enable = lib.mkEnableOption "yunirun の計測基盤";
          dir = lib.mkOption {
            type = lib.types.path;
            default = "/var/lib/yunirun-obs";
            description = "メトリクスとログの置き場所。";
          };
          alertWebhook = lib.mkOption {
            type = lib.types.str;
            default = "";
            description = ''
              アラートの送り先 (webhook)。空なら送り先を作らない。

              その先が Discord なのかメールなのかを yunirun は知らない。
              変えるときにこちらを触らずに済むよう webhook 1 つに寄せる。
            '';
          };
          retention = lib.mkOption {
            type = lib.types.str;
            default = "30d";
            description = "メトリクスとログの保持期間。";
          };
          prometheusImage = lib.mkOption { type = lib.types.str; default = ""; description = "空なら yunirun の既定値。"; };
          lokiImage = lib.mkOption { type = lib.types.str; default = ""; description = "空なら yunirun の既定値。"; };
          alloyImage = lib.mkOption { type = lib.types.str; default = ""; description = "空なら yunirun の既定値。"; };
          grafanaImage = lib.mkOption { type = lib.types.str; default = ""; description = "空なら yunirun の既定値。"; };
          nodeImage = lib.mkOption { type = lib.types.str; default = ""; description = "空なら yunirun の既定値。"; };
          tempoImage = lib.mkOption { type = lib.types.str; default = ""; description = "空なら yunirun の既定値。"; };
        };
      };
    };

    haproxyImage = lib.mkOption {
      type = lib.types.str;
      default = "";
      description = ''
        経路を担う HAProxy の image。空なら yunirun の既定値。

        distribution のパッケージではなくコンテナで動かす。配信経路に残る
        最後の「その distribution のもの」だったので、これで依存が podman と
        systemd と Quadlet だけになる。

        Prometheus exporter を持つものが要る。公式 image は USE_PROMEX=1 で
        組まれているのでそのまま使える。
      '';
    };

    dbImage = lib.mkOption {
      type = lib.types.str;
      default = "docker.io/library/postgres:18-alpine";
      description = ''
        アプリ専用 PostgreSQL の image。

        全アプリで同じものを使う。版を上げるときはここを変える。アプリごとに
        変えたい事情が出たら、そのときにアプリ側の宣言へ移す。
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
      type = lib.types.attrsOf (lib.types.submodule {
        options = {
          repo = lib.mkOption {
            type = lib.types.str;
            description = "リポジトリ (owner/name)。image の取得元になる。";
          };
          principal = lib.mkOption {
            type = lib.types.str;
            description = ''
              opkssh に渡す identity。GitHub OIDC の sub と完全一致させる。

              省略できない。かつては repo から
              repo:<owner>/<repo>:ref:refs/heads/main を導出していたが、
              その形が正しいのは「2026-07-15 より前に作られ、immutable
              subject claim へ opt-in しておらず、environment も使わない」
              場合だけになった。導出が当たるかどうかがリポジトリの生い立ちで
              決まる状態は、間違えたときに Permission denied としか出ない。

              実測してそのまま書く:
                gh api repos/<owner>/<repo>/actions/oidc/customization/sub
              が返す sub_claim_prefix に、job の性質に応じて
              :ref:refs/heads/main か :environment:<name> を足したもの。
            '';
          };
        };
      });
      default = { };
      example = {
        fighter = {
          repo = "yuniruyuni/FighterNotes";
          principal = "repo:yuniruyuni@85034901/FighterNotes@1313852776:ref:refs/heads/main";
        };
      };
      description = ''
        取り込むアプリ。名前からリポジトリと認可先への対応。

        ここに書くのは取り込みの意思決定だけで、アプリの中身に関する設定は
        各リポジトリの yunirun.jsonc にある。この一覧がそのまま opkssh の
        認可リストになるので、アプリ側が自分を勝手に取り込ませることはできない。
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    # yunirun は podman と Quadlet の上に立っている。アプリのコンテナは
    # rootless の user generator が、DB のコンテナは root 側の
    # system generator が unit に変換する。
    #
    # ここで宣言しておかないと隠れた依存になる。実際、VM テストは podman を
    # 有効にしていなかったために /etc/containers/systemd へ置いた定義が
    # unit にならず、Unit not found で落ちた。ホスト側で個別の設定を
    # したい場合に備えて mkDefault にしてある。
    virtualisation.podman.enable = lib.mkDefault true;

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
      # DB はアプリごとにコンテナとして立てるので、ホストの PostgreSQL には
      # 依存しない。image の取得に network が要る。
      #
      # network-online は after だけでなく wants も要る。順序だけ指定しても
      # target 自体が起動しないので、依存として宣言していないと警告になる。
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
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
    }) cfg.apps;

    # 取り込み一覧がそのまま認可になる。
    services.opkssh.authorizations = lib.mapAttrsToList (name: a: {
      user = "yunirun-${name}";
      principal = a.principal;
      issuer = "https://token.actions.githubusercontent.com";
    }) cfg.apps;

    # アプリごとの資源を出す口。
    #
    # cgroup を読むだけなので追加のコンテナが要らない。ホスト全体の指標では
    # 「どのアプリが食っているか」が分からず、切り分けに時間がかかる。
    systemd.services.yunirun-usage = lib.mkIf cfg.observability.enable {
      description = "yunirun: アプリごとの資源使用を出す";
      wantedBy = [ "multi-user.target" ];
      after = [ "yunirun-converge.service" ];
      path = [ pkg ];
      serviceConfig = {
        ExecStart = "${pkg}/bin/yunirun usage";
        Restart = "always";
        RestartSec = "10s";
        # 読むのは cgroup と台帳だけ。書くものは無い。
        ProtectSystem = "strict";
        ProtectHome = true;
        PrivateTmp = true;
        NoNewPrivileges = true;
      };
    };

    systemd.tmpfiles.rules = [
      # stateDir には台帳と秘密が入るので root 専用。
      "d ${cfg.stateDir} 0700 root root -"
      # ホームは stateDir の外。各アプリのホーム自体は 0700 でユーザ所有になる。
      "d ${cfg.homesDir} 0755 root root -"
      # DB のデータとソケット。ホームとは分ける (rename や remove がホームを
      # 捨てるため)。中の data は 0700 root、sock は通り抜けのため 0755。
      "d ${cfg.dbDir} 0755 root root -"
      # unit が起動時に読む env。中の各ファイルが権限を持つので、
      # ディレクトリ自体は辿れればよい。
      "d ${cfg.envDir} 0755 root root -"
      # 計測基盤のデータ。中身は :U で各コンテナのユーザへ渡る。
      "d ${cfg.observability.dir} 0755 root root -"
      "d /run/yunirun 0755 root root -"
    ];
  };
}
