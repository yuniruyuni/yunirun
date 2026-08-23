# DB を持つアプリの収束と、権限分離を確かめる e2e。
#
# ここが本番で最も壊れると困る部分。runtime が owner の資格情報を持てない
# という性質は、ファイル権限と DB のロール権限の両方に依存していて、
# どちらかが崩れても静かに成立しなくなる。
{ pkgs, self, ... }:

let
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

    services.postgresql = {
      enable = true;
      authentication = ''
        local all postgres peer
        local all all      md5
        host  all all      127.0.0.1/32 md5
      '';
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

    machine.wait_for_unit("postgresql.service")
    machine.wait_for_unit("yunirun-converge.service")

    # DB を使うと宣言したマニフェストを置いてから収束させる。
    # 実際は deploy が運んでくるが、ここでは直接置く。
    machine.succeed("mkdir -p /run/yunirun/beta/inbox")
    machine.succeed("cp ${manifest} /run/yunirun/beta/inbox/yunirun.jsonc")
    converge(machine)

    with subtest("DB とロールが作られる"):
        # 判定はゲストの中で完結させる。出力を持ち帰って照合すると、
        # ドライバとの受け渡しが稀に壊れたときに、中身ではなく経路のせいで
        # 落ちる (実際 base64 のまま返って落ちたことがある)。
        machine.succeed(
            "sudo -u postgres psql -tAc"
            " \"select 1 from pg_database where datname='beta'\" | grep -q 1"
        )
        for role in ["beta", "beta_app"]:
            machine.succeed(
                "sudo -u postgres psql -tAc"
                f" \"select 1 from pg_roles where rolname='{role}'\" | grep -q 1"
            )

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
            " psql -h /run/postgresql -U beta_app -d beta -tAc"
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
            "! sudo -u postgres psql -d beta -tAc"
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
