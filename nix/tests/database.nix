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

    services.yunirun = {
      enable = true;
      domain = "example.test";
      apps.beta = "example/beta";
    };

    virtualisation = {
      memorySize = 3072;
      diskSize = 6144;
    };
  };

  testScript = ''
    machine.wait_for_unit("postgresql.service")
    machine.wait_for_unit("yunirun-converge.service")

    # DB を使うと宣言したマニフェストを置いてから収束させる。
    # 実際は deploy が運んでくるが、ここでは直接置く。
    machine.succeed("mkdir -p /run/yunirun/beta/inbox")
    machine.succeed("cp ${manifest} /run/yunirun/beta/inbox/yunirun.jsonc")
    machine.succeed("yunirun converge")

    with subtest("DB とロールが作られる"):
        out = machine.succeed(
            "sudo -u postgres psql -tAc \"select count(*) from pg_database where datname='beta'\""
        )
        assert out.strip() == "1", f"DB が無い: {out}"
        roles = machine.succeed(
            "sudo -u postgres psql -tAc \"select rolname from pg_roles where rolname like 'beta%' order by 1\""
        )
        assert "beta" in roles and "beta_app" in roles, f"ロールが足りない: {roles}"

    with subtest("owner パスワードはアプリのユーザから読めない"):
        # これが per-workload 分離の要。runtime が owner の資格情報を持てると、
        # コンテナから脱出された際に DDL まで到達できてしまう。
        machine.fail("sudo -u yunirun-beta cat /run/yunirun/beta/migration.env")

    with subtest("自分の runtime.env は読める"):
        machine.succeed("sudo -u yunirun-beta test -r /run/yunirun/beta/runtime.env")

    with subtest("app ロールは DDL を実行できない"):
        pw = machine.succeed(
            "grep -oP '(?<=DB_PASSWORD=).*' /run/yunirun/beta/runtime.env"
        ).strip()
        # 接続はできる。
        machine.succeed(
            f"PGPASSWORD='{pw}' psql -h /run/postgresql -U beta_app -d beta -tAc 'select 1'"
        )
        # DDL はできない。pgschema が宣言する権限だけを持つ。
        machine.fail(
            f"PGPASSWORD='{pw}' psql -h /run/postgresql -U beta_app -d beta -tAc 'CREATE TABLE t(x int)'"
        )

    with subtest("per-table GRANT を撒いていない"):
        # 撒くと pgschema に毎回 REVOKE され、次の収束で復活する振動になる。
        # その間アプリは permission denied になる。
        # SQL で空文字列リテラルを使わない書き方にしてある。testScript は Nix の
        # 複数行文字列なので、単引用符を 2 つ並べると文字列終端と解釈される。
        acl = machine.succeed(
            "sudo -u postgres psql -d beta -tAc \"select count(*) from pg_default_acl\""
        )
        assert acl.strip() == "0", f"ALTER DEFAULT PRIVILEGES を発行している: {acl}"

    with subtest("パスワードは再収束で変わらない"):
        # 作り直すと DB 側と食い違い、稼働中のコンテナが認証に失敗する。
        before = machine.succeed("cat /run/yunirun/beta/runtime.env")
        machine.succeed("yunirun converge")
        after = machine.succeed("cat /run/yunirun/beta/runtime.env")
        assert before == after, "再収束でパスワードが変わった"

    with subtest("秘密は管理者鍵でも復号できる"):
        # ホストを失ったときの復旧経路。これが無いと DB へ入れなくなる。
        machine.succeed("test -f /var/lib/yunirun/secrets/beta/beta-db-owner.age")
  '';
}
