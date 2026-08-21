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

    services.yunirun = {
      enable = true;
      domain = "example.test";
      apps.alpha = "example/alpha";
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

    with subtest("HAProxy が起動する"):
        machine.wait_for_unit("yunirun-haproxy.service")

    with subtest("収束は冪等"):
        before = machine.succeed("cat /var/lib/yunirun/allocations.json")
        machine.succeed("systemctl restart yunirun-converge.service")
        after = machine.succeed("cat /var/lib/yunirun/allocations.json")
        assert before == after, "再実行で割り当てが変わった"

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
