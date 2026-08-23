# DB を持つアプリの収束と、権限分離を確かめる e2e。
#
# ここが本番で最も壊れると困る部分。runtime が owner の資格情報を持てない
# という性質は、ファイル権限と DB のロール権限の両方に依存していて、
# どちらかが崩れても静かに成立しなくなる。
{ pkgs, self, ... }:

let
  # DB の image をテスト用に組み立てる。
  #
  # 本番は docker.io/library/postgres を使うが、VM テストは外へ出られない。
  # Docker Hub からダイジェスト固定で取ると、テストが外部の可用性に依存する
  # うえ、更新のたびにハッシュを書き換えることになる。
  #
  # nixpkgs の postgresql に、公式 image と同じ入口 (POSTGRES_USER /
  # POSTGRES_PASSWORD / POSTGRES_DB を読んで初期化する) だけを付けたものを
  # 使う。これで確かめられるのは yunirun 側の振る舞い — unit の生成、
  # 起動の順序、ロールの作成、権限の分離 — で、そこが目的。
  #
  # 公式 image の PGDATA の置き場所までは再現しないので、そこが変わった場合は
  # このテストでは捕まらない。
  # 公式 image と同じ入口だけを持たせる。root で起動して postgres へ権限を
  # 落とすところまで含める。initdb は root では動かないため、ここを省くと
  # ソケットが現れないまま「起動している」ように見える。
  entrypoint = pkgs.writeShellScript "pg-entrypoint" ''
    set -eu
    export PATH=${pkgs.postgresql}/bin:${pkgs.coreutils}/bin:${pkgs.util-linux}/bin:$PATH
    export PGDATA=/var/lib/postgresql/data

    if [ "$(id -u)" = 0 ]; then
      mkdir -p "$PGDATA" /var/run/postgresql
      chown -R 999:999 /var/lib/postgresql /var/run/postgresql
      exec setpriv --reuid 999 --regid 999 --clear-groups "$0" "$@"
    fi

    chmod 700 "$PGDATA"
    # 初期化が途中で落ちると PG_VERSION だけ出来て DB が無い状態になり、
    # 次の起動が「初期化済み」と誤判定する。完了の印を別に置く。
    if [ ! -f "$PGDATA/.initialized" ]; then
      rm -rf "$PGDATA"/* "$PGDATA"/.[!.]* 2>/dev/null || true
      printf '%s' "$POSTGRES_PASSWORD" > /tmp/pw
      initdb -U "$POSTGRES_USER" --pwfile=/tmp/pw --auth-local=md5 --auth-host=md5
      rm -f /tmp/pw
      pg_ctl -D "$PGDATA" -o "-c listen_addresses= -c unix_socket_directories=/var/run/postgresql" -w start
      # auth-local を md5 にしてあるので、ローカル接続にもパスワードが要る。
      # 渡さないと createdb が失敗し、しかも PG_VERSION は既に出来ているので
      # 次の起動が「初期化済み」と判定して DB だけ無い状態になる。
      PGPASSWORD="$POSTGRES_PASSWORD" createdb -h /var/run/postgresql \
        -U "$POSTGRES_USER" -O "$POSTGRES_USER" "$POSTGRES_DB"
      pg_ctl -D "$PGDATA" -w stop
      touch "$PGDATA/.initialized"
    fi
    exec postgres -c listen_addresses= -c unix_socket_directories=/var/run/postgresql "$@"
  '';

  dbImage = pkgs.dockerTools.buildImage {
    name = "localhost/postgres-test";
    tag = "latest";
    copyToRoot = pkgs.buildEnv {
      name = "pg-root";
      paths = [ pkgs.postgresql pkgs.coreutils pkgs.bash pkgs.util-linux ];
      pathsToLink = [ "/bin" ];
    };
    runAsRoot = ''
      #!${pkgs.runtimeShell}
      mkdir -p /var/lib/postgresql /var/run/postgresql /etc /tmp
      chmod 1777 /tmp
      printf 'root:x:0:0::/root:/bin/sh\npostgres:x:999:999::/var/lib/postgresql:/bin/sh\n' > /etc/passwd
      printf 'root:x:0:\npostgres:x:999:\n' > /etc/group
      chown -R 999:999 /var/lib/postgresql /var/run/postgresql
    '';
    config.Entrypoint = [ "${entrypoint}" ];
  };

  # マニフェストは Nix 側でファイルにする。テストスクリプト内でヒアドキュメントを
  # 書くと、Python の三重引用符が Nix の文字列終端 '' と衝突する。
  manifest = pkgs.writeText "yunirun.jsonc" (builtins.toJSON {
    app.database = true;
    workloads.migration = { };
  });
in
pkgs.testers.runNixOSTest {
  name = "yunirun-database";

  nodes.machine = { pkgs, ... }: {
    imports = [ self.nixosModules.default ];

    system.activationScripts.testAgeKey = ''
      mkdir -p /var/lib/agenix
      if [ ! -f /var/lib/agenix/age-key.txt ]; then
        ${pkgs.age}/bin/age-keygen -o /var/lib/agenix/age-key.txt 2>/dev/null
      fi
    '';

    # ホストの PostgreSQL は要らない。DB はアプリごとにコンテナで立てる。
    # psql は yunirun がロールを作るのに使うので、パッケージだけ入れる。
    environment.systemPackages = [ pkgs.postgresql ];

    # VM に外向きのネットワークが無いので、image を事前に読み込ませる。
    services.yunirun.dbImage = "localhost/postgres-test:latest";
    systemd.services.load-db-image = {
      wantedBy = [ "yunirun-converge.service" ];
      before = [ "yunirun-converge.service" ];
      path = [ pkgs.podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.podman}/bin/podman load -i ${dbImage}";
      };
    };

    # HAProxy のログをコンソールへ流さない。
    #
    # nixos テストのドライバはコマンドの出力をコンソール越しに読む。converge が
    # HAProxy を読み直すと、その瞬間に backend has no server available が
    # 出力へ割り込み、ドライバが結果を base64 として復号できずに落ちる
    # (Incorrect padding)。journal には残るので調査には困らない。
    services.journald.extraConfig = "ForwardToConsole=no";

    services.yunirun = {
      enable = true;
      domain = "example.test";
      apps.beta = {
        repo = "example/beta";
        principal = "repo:example/beta:ref:refs/heads/main";
      };
    };

    virtualisation = {
      memorySize = 3072;
      diskSize = 6144;
    };
  };

  testScript = ''
    # converge は端末を握ったまま子プロセスを起こす。それが後から書き込むと
    # ドライバがコマンド出力を base64 として復号できずに落ちる
    # (Incorrect padding)。中身とは無関係に落ちるので、標準入出力を端末から
    # 切り離して呼ぶ。失敗したときの診断はファイルに残す。
    def converge(m, cmd="yunirun converge"):
        m.succeed(
            cmd + " </dev/null >/tmp/converge.log 2>&1"
            " || { cat /tmp/converge.log; exit 1; }"
        )

    machine.wait_for_unit("yunirun-converge.service")

    # アプリ専用 PostgreSQL への接続。owner の資格情報は root しか読めない
    # migration.env にある。ホストの psql からソケット越しに繋ぐ。
    SOCK = "/var/lib/yunirun-db/beta/sock"
    OWNER = (
        "PGPASSWORD=$(grep -oP '(?<=DB_PASSWORD=).*'"
        " /run/yunirun/beta/migration.env)"
        f" psql -h {SOCK} -U beta"
    )

    # DB を使うと宣言したマニフェストを置いてから収束させる。
    # 実際は deploy が運んでくるが、ここでは直接置く。
    machine.succeed("mkdir -p /run/yunirun/beta/inbox")
    machine.succeed("cp ${manifest} /run/yunirun/beta/inbox/yunirun.jsonc")
    converge(machine)

    with subtest("アプリ専用の DB が立つ"):
        # 落ちたときに中身が分からないと調べようがないので、DB のログを出す。
        try:
            machine.wait_for_unit("beta-db.service")
        except Exception:
            machine.execute("journalctl -u beta-db.service --no-pager | tail -40 >&2")
            raise
        # 判定はゲストの中で完結させる。出力を持ち帰って照合すると、
        # ドライバとの受け渡しが稀に壊れたときに、中身ではなく経路のせいで
        # 落ちる (実際 base64 のまま返って落ちたことがある)。
        machine.succeed(
            f"{OWNER} -d beta -tAc"
            " \"select 1 from pg_database where datname='beta'\" | grep -q 1"
        )
        for role in ["beta", "beta_app"]:
            machine.succeed(
                f"{OWNER} -d beta -tAc"
                f" \"select 1 from pg_roles where rolname='{role}'\" | grep -q 1"
            )

    with subtest("DB は到達経路を持たない"):
        # ネットワークを与えると同じホスト上の他のコンテナから届きうる。
        machine.succeed("grep -q 'Network=none' /etc/containers/systemd/beta-db.container")
        machine.succeed("! grep -q PublishPort /etc/containers/systemd/beta-db.container")

    with subtest("データはホームの外に置かれる"):
        # ホームは rename や remove が捨てる。そこに置くと名前を変えただけで
        # データが消える。
        machine.succeed("test -d /var/lib/yunirun-db/beta/data")
        machine.succeed("! test -e /var/lib/yunirun-apps/beta/pgdata")

    with subtest("owner の資格情報はアプリのユーザから読めない"):
        # DB コンテナは root 側の Quadlet として動かす。アプリのユーザで
        # 動かすと、初期化に要る owner のパスワードをそのユーザが読める。
        machine.fail("sudo -u yunirun-beta cat /run/yunirun/beta/db.env")
        machine.succeed("test -f /etc/containers/systemd/beta-db.container")

    with subtest("owner パスワードはアプリのユーザから読めない"):
        # これが per-workload 分離の要。runtime が owner の資格情報を持てると、
        # コンテナから脱出された際に DDL まで到達できてしまう。
        machine.fail("sudo -u yunirun-beta cat /run/yunirun/beta/migration.env")

    with subtest("自分の runtime.env は読める"):
        machine.succeed("sudo -u yunirun-beta test -r /run/yunirun/beta/runtime.env")

    with subtest("app ロールは DDL を実行できない"):
        # パスワードはゲストの中だけで扱う。ドライバへ持ち帰ると、テストの
        # 出力や失敗時の表示に平文で乗る。
        conn = (
            "PGPASSWORD=$(grep -oP '(?<=DB_PASSWORD=).*'"
            " /run/yunirun/beta/runtime.env)"
            f" psql -h {SOCK} -U beta_app -d beta -tAc"
        )
        # 接続はできる。
        machine.succeed(f"{conn} 'select 1'")
        # DDL はできない。pgschema が宣言する権限だけを持つ。
        machine.fail(f"{conn} 'CREATE TABLE t(x int)'")

    with subtest("per-table GRANT を撒いていない"):
        # 撒くと pgschema に毎回 REVOKE され、次の収束で復活する振動になる。
        # その間アプリは permission denied になる。
        # SQL で空文字列リテラルを使わない書き方にしてある。testScript は Nix の
        # 複数行文字列なので、単引用符を 2 つ並べると文字列終端と解釈される。
        machine.succeed(
            f"! {OWNER} -d beta -tAc"
            " \"select 1 from pg_default_acl\" | grep -q 1"
        )

    with subtest("パスワードは再収束で変わらない"):
        # 作り直すと DB 側と食い違い、稼働中のコンテナが認証に失敗する。
        # 比較もゲストの中で行う。パスワードを持ち出さずに済む。
        machine.succeed("cp /run/yunirun/beta/runtime.env /tmp/before.env")
        converge(machine)
        machine.succeed(
            "cmp -s /tmp/before.env /run/yunirun/beta/runtime.env"
            " || { echo '再収束でパスワードが変わった'; exit 1; }"
        )

    with subtest("秘密は管理者鍵でも復号できる"):
        # ホストを失ったときの復旧経路。これが無いと DB へ入れなくなる。
        machine.succeed("test -f /var/lib/yunirun/secrets/beta/beta-db-owner.age")
  '';
}
