# VM テスト用の HAProxy image。
#
# VM は外へ出られないので、公式 image を引く代わりにここで組み立てる。
# nixpkgs の haproxy にも USE_PROMEX=yes が入っているので、計測の口まで
# 同じように試せる。
{ pkgs }:

pkgs.dockerTools.buildImage {
  name = "localhost/haproxy-test";
  tag = "latest";
  copyToRoot = pkgs.buildEnv {
    name = "haproxy-root";
    paths = [ pkgs.haproxy pkgs.coreutils ];
    pathsToLink = [ "/bin" ];
  };
  runAsRoot = ''
    #!${pkgs.runtimeShell}
    mkdir -p /tmp /etc
    chmod 1777 /tmp
    printf 'root:x:0:0::/root:/bin/sh\n' > /etc/passwd
    printf 'root:x:0:\n' > /etc/group
  '';
}
