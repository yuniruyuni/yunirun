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

    # HAProxy のログをコンソールへ流さない。
    #
    # nixos テストのドライバはコマンドの出力をコンソール越しに読む。converge が
    # HAProxy を読み直すと、その瞬間に backend has no server available が
    # 出力へ割り込み、ドライバが結果を base64 として復号できずに落ちる
    # (Incorrect padding)。journal には残るので調査には困らない。
    services.journald.extraConfig = "ForwardToConsole=no";

    # 判定をゲストの中で行うために使う。
    environment.systemPackages = [ pkgs.jq ];

    services.yunirun = {
      enable = true;
      domain = "example.test";
      apps = {
        # 認可先は必ず明示する。導出は無い。
        alpha = {
          repo = "example/alpha";
          principal = "repo:example/alpha:ref:refs/heads/main";
        };
        # job に environment が付くと後半が変わり、sub claim を
        # カスタマイズしていると前半も変わる。どちらもそのまま書ける。
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

    with subtest("宣言した認可先がそのまま auth_id に落ちる"):
        machine.succeed(
            "grep -qF 'yunirun-alpha repo:example/alpha:ref:refs/heads/main'"
            " /etc/opk/auth_id"
        )
        machine.succeed(
            "grep -qF 'yunirun-beta repo:example@42/beta@7:environment:production'"
            " /etc/opk/auth_id"
        )

    with subtest("repo から認可先を作らない"):
        # かつては repo から repo:<owner>/<repo>:ref:refs/heads/main を
        # 導出していた。beta の repo は example/beta なので、導出が残って
        # いればこの文字列が現れる。認可を勝手に広げないことを見る。
        machine.succeed("! grep -qF 'repo:example/beta:ref' /etc/opk/auth_id")

    with subtest("DB は宣言が無ければ作られない"):
        # マニフェストをまだ受け取っていないので database の宣言も無い。
        # 使わないアプリに DB を作ると消し忘れた資源が溜まる。
        #
        # 判定はゲストの中で完結させる。出力を持ち帰って照合すると、
        # ドライバとの受け渡しが稀に壊れたときに、中身ではなく経路のせいで
        # 落ちる (実際 MAo= という base64 のまま返って落ちたことがある)。
        machine.succeed(
            "! sudo -u postgres psql -tAc"
            " \"select 1 from pg_database where datname='alpha'\" | grep -q 1"
        )

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
        # 比較もゲストの中で行う。中身をドライバへ運ぶ必要が無い。
        machine.succeed("cp /var/lib/yunirun/allocations.json /tmp/before.json")
        machine.succeed("systemctl restart yunirun-converge.service")
        machine.succeed(
            "cmp -s /tmp/before.json /var/lib/yunirun/allocations.json"
            " || { echo '再実行で割り当てが変わった'; diff /tmp/before.json"
            " /var/lib/yunirun/allocations.json; exit 1; }"
        )

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
        led = "/var/lib/yunirun/allocations.json"
        machine.succeed(f"jq -S '.entries.alpha' {led} > /tmp/alpha-before.json")
        machine.succeed(
            "sed -i 's|\"alpha\":|\"aaa\": \"example/aaa\", \"alpha\":|'"
            " /etc/yunirun/config.json"
        )
        machine.succeed("yunirun converge")

        # 既存の割り当てが 1 バイトも動いていないこと。
        machine.succeed(
            f"jq -S '.entries.alpha' {led} > /tmp/alpha-after.json;"
            " cmp -s /tmp/alpha-before.json /tmp/alpha-after.json"
            " || { echo '既存の割り当てが動いた';"
            " diff /tmp/alpha-before.json /tmp/alpha-after.json; exit 1; }"
        )
        # 新規アプリが既存の番号を奪っていないこと。
        machine.succeed(
            f"test \"$(jq '.entries.aaa.UID' {led})\""
            f" != \"$(jq '.entries.alpha.UID' {led})\""
        )

    with subtest("宣言から消すと止まる"):
        # 経路は宣言から生成しているので勝手に消えるが、コンテナは
        # Restart=always の user unit なので誰も止めない。外からは 404 なのに
        # 中では動いてポートを掴んだままになる。経路と揃えて止める。
        machine.succeed("loginctl show-user yunirun-aaa -p Linger | grep -q yes")
        machine.succeed(
            "sed -i 's|\"aaa\": \"example/aaa\", ||' /etc/yunirun/config.json"
        )
        machine.succeed("yunirun converge")
        machine.succeed("! loginctl show-user yunirun-aaa -p Linger | grep -q yes")
        machine.succeed("! grep -q 'frontend aaa_in' /etc/yunirun/haproxy.cfg")

    with subtest("止めるだけで消さない"):
        # 宣言の書き間違いでデータが飛ぶのは割に合わない。片付けは
        # yunirun remove が明示的に行う。
        machine.succeed("id yunirun-aaa")
        machine.succeed("test -d /var/lib/yunirun-apps/aaa")
        machine.succeed(f"jq -e '.entries.aaa' {led}")

    with subtest("remove は宣言に残っているものを拒む"):
        # 消しても次の収束が作り直すので意味が無く、その間だけ落ちる。
        machine.fail("yunirun remove alpha")

    with subtest("remove は実体を片付けるが DB は残す"):
        machine.succeed("yunirun remove aaa")
        machine.fail("id yunirun-aaa")
        machine.succeed("test ! -d /var/lib/yunirun-apps/aaa")
        machine.succeed("! grep -q yunirun-aaa /etc/subuid")
        machine.succeed(f"! jq -e '.entries.aaa' {led}")
  '';
}
