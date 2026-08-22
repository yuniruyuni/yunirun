# yunirun を実際の NixOS 上で動かす e2e テスト。
#
# 単体テストでは押さえられない部分を確かめる。ここで検証するのは、これまで
# 実際に本番で踏んだ事柄そのもの:
#
#   - converge がユーザ・DB・秘密・unit を作れるか
#   - 秘密の権限が意図どおりか (runtime は owner パスワードを読めない)
#   - アプリを追加しても既存の割り当てが動かないか
#   - 収束が冪等か
#
# これらは systemd・PostgreSQL・podman が揃った実機でないと確かめられない。
{ pkgs, self, ... }:

pkgs.testers.runNixOSTest {
  name = "yunirun-converge";

  nodes.machine = { config, pkgs, ... }: {
    imports = [ self.nixosModules.default ];

    # テスト用のホスト鍵。実運用では ssh のホスト鍵から導く。
    system.activationScripts.testAgeKey = ''
      mkdir -p /var/lib/agenix
      if [ ! -f /var/lib/agenix/age-key.txt ]; then
        ${pkgs.age}/bin/age-keygen -o /var/lib/agenix/age-key.txt 2>/dev/null
      fi
    '';

    services.postgresql = {
      enable = true;
      # コンテナから Unix ソケット経由で繋ぐために必要な設定。本番と揃える。
      authentication = ''
        local all postgres peer
        local all all      md5
        host  all all      127.0.0.1/32 md5
      '';
    };

    # 認可リストが実際に auth_id へ落ちることを見たいので opkssh を有効にする。
    # yunirun 側は services.opkssh.authorizations を書くだけなので、これが
    # 無いと生成物を確かめられない。
    services.openssh.enable = true;
    services.opkssh.enable = true;

    services.yunirun = {
      enable = true;
      domain = "example.test";
      apps = {
        # 省略形。認可先は repo から導かれる。
        alpha = "example/alpha";
        # attrset 形。sub claim をカスタマイズしているリポジトリ向けに
        # 認可先を明示できることを確かめる。
        beta = {
          repo = "example/beta";
          principal = "repo:example@42/beta@7:environment:production";
        };
      };
    };

    virtualisation = {
      memorySize = 3072;
      diskSize = 6144;
    };
  };

  testScript = ''
    machine.wait_for_unit("postgresql.service")
    machine.wait_for_unit("yunirun-converge.service")

    with subtest("ユーザが作られる"):
        machine.succeed("id yunirun-alpha")
        machine.succeed("id yunirun-beta")

    with subtest("認可先は省略形なら repo から導かれる"):
        auth = machine.succeed("cat /etc/opk/auth_id")
        assert "yunirun-alpha repo:example/alpha:ref:refs/heads/main" in auth, auth

    with subtest("認可先は attrset で明示できる"):
        # sub claim をカスタマイズしているリポジトリは導出では追いつかない。
        # 導出値が混ざっていないことまで見る。
        auth = machine.succeed("cat /etc/opk/auth_id")
        assert "yunirun-beta repo:example@42/beta@7:environment:production" in auth, auth
        assert "repo:example/beta:ref" not in auth, auth

    with subtest("DB は宣言が無ければ作られない"):
        # マニフェストをまだ受け取っていないので database の宣言も無い。
        # 使わないアプリに DB を作ると消し忘れた資源が溜まる。
        out = machine.succeed(
            "sudo -u postgres psql -tAc \"select count(*) from pg_database where datname='alpha'\""
        )
        assert out.strip() == "0", f"DB が作られている: {out}"

    with subtest("unit が置かれる"):
        machine.succeed("test -f /var/lib/yunirun-apps/alpha/.config/containers/systemd/alpha-blue.container")
        machine.succeed("test -f /var/lib/yunirun-apps/alpha/.config/containers/systemd/alpha-green.container")
        machine.succeed("test -f /var/lib/yunirun-apps/beta/.config/containers/systemd/beta-blue.container")

    with subtest("HAProxy が起動する"):
        machine.wait_for_unit("yunirun-haproxy.service")

    with subtest("HAProxy は書いた設定を実際に配っている"):
        # 設定を書くだけで読み直させないと、ディスク上の内容と動いている
        # 内容が食い違ったまま気付けない。宣言したアプリの frontend が
        # 実際に listen されていることで確かめる。
        for app in ["alpha", "beta"]:
            machine.succeed(f"grep -q 'frontend {app}_in' /etc/yunirun/haproxy.cfg")
        machine.wait_until_succeeds("ss -tln | grep -q 127.0.0.1:8100")
        machine.wait_until_succeeds("ss -tln | grep -q 127.0.0.1:8110")

    with subtest("収束は冪等"):
        before = machine.succeed("cat /var/lib/yunirun/allocations.json")
        machine.succeed("systemctl restart yunirun-converge.service")
        after = machine.succeed("cat /var/lib/yunirun/allocations.json")
        assert before == after, "再実行で割り当てが変わった"

    with subtest("前提が実際の環境で成り立っている"):
        # yunirun は外部システムの挙動に多く依存している。実際に踏んだ不具合の
        # 大半は「外部の仕様を知らなかった」ことに起因していた。前提を明示して、
        # それが常識的なシステム構成で成り立つことをここで確かめる。
        machine.succeed("yunirun doctor")
        # アプリのユーザとしても検査する。ファイルの到達性や XDG_RUNTIME_DIR は
        # ユーザによって変わる。
        machine.succeed("yunirun doctor --app alpha")

    with subtest("アプリを足しても既存の割り当てが動かない"):
        # 名前順のインデックスから導出していた頃、アルファベット順で前に入る
        # 名前を足すと既存アプリの uid とポートが全てずれた。稼働中の
        # コンテナは旧ポートのままで HAProxy は新ポートを見るため停止する。
        import json
        before = json.loads(machine.succeed("cat /var/lib/yunirun/allocations.json"))
        machine.succeed(
            "sed -i 's|\"alpha\":|\"aaa\": \"example/aaa\", \"alpha\":|' /etc/yunirun/config.json"
        )
        machine.succeed("yunirun converge")
        after = json.loads(machine.succeed("cat /var/lib/yunirun/allocations.json"))
        assert after["entries"]["alpha"] == before["entries"]["alpha"], (
            f"既存の割り当てが動いた: {before['entries']['alpha']} -> {after['entries']['alpha']}"
        )
        assert after["entries"]["aaa"]["UID"] != before["entries"]["alpha"]["UID"], (
            "新規アプリが既存の番号を奪った"
        )
  '';
}
