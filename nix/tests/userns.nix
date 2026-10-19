# 非 root の image を動かすアプリが、activation と再起動を跨いで動き続けるかの e2e。
#
# 本番で踏んだ障害をそのまま再現する:
#
#   ホストが再起動すると、NixOS の activation が /etc/subuid を宣言から作り
#   直して yunirun の行を消した。converge が戻すより先に linger のユーザの
#   unit が起動し、podman が行の無い状態でユーザ名前空間 (pause プロセス) を
#   作った。行が戻った後もその名前空間が使われ続け、非 root (uid 65532) の
#   image が "insufficient UIDs or GIDs available in user namespace" で
#   展開できずに落ちた。root の image は 1 個の写像でも動くので、影響は
#   fighter (distroless nonroot) と lom (nginx) だけだった。doctor も
#   すべて成り立っていると報告した。
#
# ここで確かめること:
#   - activation を走らせ直しても subuid/subgid の行が残る
#   - 行が欠けた状態で作られた名前空間を doctor が見つける
#   - converge がそれを作り直し、アプリが再び応答する
#   - 再起動を跨いでも、作り直しを要さずにアプリが応答する
{ pkgs, self, ... }:

let
  haproxyImage = import ./haproxy-image.nix { inherit pkgs; };

  # distroless nonroot と同じ形の image。uid 65532 が所有するファイルを持ち、
  # その uid で動く。展開の時点で 65532 を写像できないと lchown に失敗する。
  #
  # unit が参照する localhost/<app>:current の名前で組むので、deploy (GHCR
  # からの取得) を通さずに読み込める。
  appImage = pkgs.dockerTools.buildImage {
    name = "localhost/alpha";
    tag = "current";
    copyToRoot = pkgs.buildEnv {
      name = "nonroot-root";
      paths = [ pkgs.busybox ];
      pathsToLink = [ "/bin" ];
    };
    runAsRoot = ''
      #!${pkgs.runtimeShell}
      mkdir -p /app /tmp
      chmod 1777 /tmp
      echo ok > /app/health
      chown -R 65532:65532 /app
    '';
    config = {
      User = "65532:65532";
      WorkingDir = "/app";
      Cmd = [ "/bin/httpd" "-f" "-p" "3000" "-h" "/app" ];
    };
  };
in
pkgs.testers.runNixOSTest {
  name = "yunirun-userns";

  nodes.machine = { pkgs, ... }: {
    imports = [ self.nixosModules.default ];

    system.activationScripts.testAgeKey = ''
      mkdir -p /var/lib/agenix
      if [ ! -f /var/lib/agenix/age-key.txt ]; then
        ${pkgs.age}/bin/age-keygen -o /var/lib/agenix/age-key.txt 2>/dev/null
      fi
    '';

    # HAProxy のログをコンソールへ流さない (converge.nix と同じ理由)。
    services.journald.extraConfig = "ForwardToConsole=no";

    environment.systemPackages = [ pkgs.curl ];

    # VM に外向きのネットワークが無いので、image を事前に読み込ませる。
    systemd.services.load-haproxy-image = {
      wantedBy = [ "yunirun-converge.service" ];
      before = [ "yunirun-converge.service" ];
      path = [ pkgs.podman ];
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = "${pkgs.podman}/bin/podman load -i ${haproxyImage}";
      };
    };

    services.yunirun = {
      enable = true;
      domain = "example.test";
      haproxyImage = "localhost/haproxy-test:latest";
      apps.alpha = {
        repo = "example/alpha";
        principal = "repo:example/alpha:ref:refs/heads/main";
      };
    };

    virtualisation = {
      memorySize = 3072;
      diskSize = 6144;
    };
  };

  testScript = ''
    def converge(m, cmd="yunirun converge"):
        m.succeed(
            cmd + " </dev/null >/tmp/converge.log 2>&1"
            " || { cat /tmp/converge.log; exit 1; }"
        )

    # 割り当ては台帳の先頭 (index 0)。
    SUBUID = 4000000
    LINE = f"yunirun-alpha:{SUBUID}:65536"
    BLUE = 8101

    def wait_converge(m):
        # 落ちた理由は journal にしか出ない (コンソールへは流していない)。
        try:
            m.wait_for_unit("yunirun-converge.service")
        except Exception:
            m.execute("journalctl -u yunirun-converge.service -b --no-pager | tail -60 >&2")
            raise

    wait_converge(machine)
    UID = machine.succeed("id -u yunirun-alpha").strip()
    PAUSE = f"/run/user/{UID}/libpod/tmp/pause.pid"

    def as_alpha(cmd):
        # yunirun と同じ経路でアプリのユーザとして実行する。
        return (
            f"systemd-run --uid {UID} --setenv XDG_RUNTIME_DIR=/run/user/{UID}"
            " --setenv PATH=/run/wrappers/bin:/run/current-system/sw/bin"
            f" --pipe --wait --quiet --collect {cmd}"
        )

    def serving():
        machine.wait_until_succeeds(
            f"curl -sf http://127.0.0.1:{BLUE}/health | grep -qx ok", timeout=180
        )

    with subtest("subuid の行が置かれる"):
        machine.succeed(f"grep -qx {LINE} /etc/subuid")
        machine.succeed(f"grep -qx {LINE} /etc/subgid")

    with subtest("非 root (uid 65532) の image が動く"):
        # 展開の時点で 65532 を写像する必要がある。名前空間が正しくないと
        # ここで lchown に失敗する。
        machine.succeed(as_alpha("podman load -i ${appImage}"))
        # unit は Restart=always で image を待っているので、そのうち上がる。
        serving()

    with subtest("activation を走らせ直しても行が残る"):
        # activation が本当に /etc/subuid を作り直していることを確かめるため、
        # 宣言に無い行を足しておく。これが消えれば作り直しが起きている。
        machine.succeed("echo 'not-declared:5000000:65536' >> /etc/subuid")
        machine.succeed(
            "/run/current-system/activate >/tmp/activate.log 2>&1"
            " || { cat /tmp/activate.log; exit 1; }"
        )
        machine.succeed("! grep -q not-declared /etc/subuid")
        machine.succeed(f"grep -qx {LINE} /etc/subuid")
        machine.succeed(f"grep -qx {LINE} /etc/subgid")
        machine.succeed("yunirun doctor")
        serving()

    with subtest("行が欠けた間に作られた名前空間を doctor が見つける"):
        # 修正前の再起動を再現する。ユーザの unit が止まっている間に行が消え、
        # その状態で unit が起動して podman が名前空間を作る。
        machine.succeed(f"systemctl stop user@{UID}.service")
        machine.succeed("pkill -9 -u yunirun-alpha || true")
        machine.succeed(f"rm -f {PAUSE}")
        machine.succeed("sed -i '/^yunirun-alpha:/d' /etc/subuid /etc/subgid")
        machine.succeed(f"systemctl start user@{UID}.service")
        # unit が podman を動かし、行の無い名前空間ができるのを待つ。
        machine.wait_until_succeeds(
            f"p=$(cat {PAUSE}) && kill -0 $p && ! grep -q {SUBUID} /proc/$p/uid_map",
            timeout=120,
        )

        # 修正前の converge と同じく、行だけを戻す。
        machine.succeed("yunirun subids")
        machine.succeed(f"grep -qx {LINE} /etc/subuid")

        # 行が戻っても、名前空間は作った時のまま。
        machine.succeed(f"! grep -q {SUBUID} /proc/$(cat {PAUSE})/uid_map")
        # 非 root の image はこの名前空間では動かない (本番の症状)。
        machine.fail(f"curl -sf http://127.0.0.1:{BLUE}/health")

        # /etc/subuid を見るだけの検査はこれを見逃していた。
        machine.fail("yunirun doctor >/tmp/doctor.log 2>&1")
        machine.succeed("grep -q podman/user-namespace-is-fixed-when-created /tmp/doctor.log")
        machine.fail("yunirun doctor --app alpha >/tmp/doctor.log 2>&1")
        machine.succeed("grep -q podman/user-namespace-is-fixed-when-created /tmp/doctor.log")

    with subtest("converge が名前空間を作り直す"):
        converge(machine)
        machine.succeed("grep -q 'podman の名前空間' /tmp/converge.log")
        # pause プロセスは作り直しの途中で入れ替わりうるので、落ち着くのを待つ。
        machine.wait_until_succeeds(f"grep -q {SUBUID} /proc/$(cat {PAUSE})/uid_map", timeout=60)
        machine.succeed("yunirun doctor")
        # image は取り直さなくてよい。保存済みの層は正しい写像で展開されている。
        serving()

    with subtest("再起動を跨いで動き続ける"):
        machine.shutdown()
        machine.start()
        wait_converge(machine)
        machine.succeed(f"grep -qx {LINE} /etc/subuid")
        serving()
        # 起動時の activation で行が戻っているので、converge が作り直す
        # 必要は無かったはず。作り直していれば、ユーザの unit が欠けた
        # 状態を見たということになる。
        machine.succeed(
            "! journalctl -b -u yunirun-converge.service --no-pager"
            " | grep -q 'podman の名前空間'"
        )
        # pause プロセスは作り直しの途中で入れ替わりうるので、落ち着くのを待つ。
        machine.wait_until_succeeds(f"grep -q {SUBUID} /proc/$(cat {PAUSE})/uid_map", timeout=60)
        machine.succeed("yunirun doctor")
  '';
}
