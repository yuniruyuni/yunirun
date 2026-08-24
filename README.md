# yunirun

`yuniruyuni.net` の VPS 上でコンテナ化したアプリを動かすための小さなデプロイシステム。

Cloud Run から移行するために作った。**NixOS に依存しない**独立したツールで、
NixOS 側にはこれを設置・設定するモジュールがあるだけ。

## 何をするか

```
GitHub Actions (アプリ repo)
  │  opkssh で短命の SSH 証明書を取得 (長期の秘密鍵を置かない)
  ▼
yunirun deploy <sha>        ← アプリの deploy ユーザとして実行
  │  GHCR から pull → schema 適用 → blue/green を片方ずつ入替
  ▼
HAProxy がヘルスチェックで振り分けを追従
  ▼
cloudflared → 公開ホスト名
```

## 設定がどこにあるか

**3 層に分かれている。** 情報を持つべき場所に持たせるのが設計の中心。

### 1. システム側 (NixOS の宣言)

取り込みの意思決定だけ。**アプリ側が自分を勝手に取り込ませることはできない。**

```nix
services.yunirun.apps.fighter = {
  repo = "yuniruyuni/FighterNotes";
  # opkssh に渡す identity。GitHub OIDC の sub と完全一致させる。
  #   gh api repos/<owner>/<repo>/actions/oidc/customization/sub
  principal = "repo:yuniruyuni@85034901/FighterNotes@1313852776:ref:refs/heads/main";
};
```

`principal` は省略できない。かつては repo から導出していたが、その形が
正しいのは「2026-07-15 より前に作られ、immutable subject claim へ opt-in
しておらず、environment も使わない」場合だけになった。導出が当たるかどうかが
リポジトリの生い立ちで決まる状態は、間違えたときに `Permission denied` としか
出ないため、書かせる方を選んでいる。

モジュールはここから 2 つのものを作る。**認可 (`/etc/opk/auth_id`)** と、
yunirun 本体が読む **`/etc/yunirun/config.json`**。後者に principal は
入らない。認可は NixOS 側の仕事で、yunirun が知る必要がない。

```json
{
  "domain": "yuniruyuni.net",
  "apps": { "fighter": "yuniruyuni/FighterNotes" }
}
```

### 2. yunirun が導出するもの

uid/gid、ホストポート、unit 名、HAProxy backend 名、DB 名、ロール名、
そして DB パスワード。

**アプリが知る必要も宣言する必要もない、純粋なホスト側の事情。**

これらを人が書くと番号を重複させる。この設定を手書きしていた時期に uid を
既存ユーザと衝突させ、グループが共有されてしまったことがあった (発見して修正済み)。
`internal/alloc` に閉じ込めて名前から一意に導出し、人が触れないようにしてある。
重複しないことと、NixOS が動的割り当てに使う帯を避けることはテストで固定した。

DB パスワードは yunirun が生成する。この値はマシンの外に出る必要が無いので、
人が管理する理由が無い。ホスト鍵と管理者鍵に対して age 暗号化して保存する。

### 3. アプリリポジトリ (`yunirun.jsonc`)

アプリだけが知っていること。**多くのアプリでは不要**で、既定値で足りる。

```jsonc
{
  // 既定は 3000 / "/health"。nginx のように PORT を見ないものだけ書く
  "app": { "port": 80, "health": "/" },

  "workloads": {
    "migration": { "image": "fighter-migration" },
    "cleanup": {
      "schedule": "02:23",
      "args": ["--batch=cleanup"],
      // app.env はワークロードにも渡る。違う値が要るものだけここに書く。
      "env": { "PGPOOL_MAX": "1", "CLEANUP_BATCH_SIZE": "500" }
    }
  }
}
```

秘密は `secrets/<ENV_NAME>.age` というファイル名自体が宣言を兼ねるので、
ここに列挙しない。deploy が暗号文のまま運び、復号は converge が行う。

暗号化の宛先は `yunirun recipient` で表示できる。

```sh
age -a -r "$(ssh yuniruyuni.net sudo yunirun recipient)" \
  > secrets/BETTER_AUTH_SECRET.age
```

この鍵はホスト鍵とは別に持つ。ホスト鍵は ssh のホスト鍵から導いているので
ホストを作り直すと変わるが、アプリのリポジトリにある暗号文は人が暗号化した
もので、鍵が変わると全アプリで暗号化し直しになる。

**マニフェストはデプロイ時に運ばれる。** VPS が GitHub を取りに行かないので、
private リポジトリでも認証情報を置かずに済む。

## 設計上の決めごと

- **コンテナは 127.0.0.1 にのみ publish する。** 外部からの到達は Cloudflare
  Tunnel 経由に限る
- **PostgreSQL へは Unix ソケットで繋ぐ。** コンテナは独立した netns に置き、
  TCP を使わないことでホストの loopback 上の他サービスへ到達できないようにする
- **HAProxy もコンテナで動かす。** distribution のパッケージに依存すると、
  そこだけ移植性が切れる。設定は converge が生成し、`podman kill --signal USR2`
  で読み直させる
- **各系の生死は計測の口から見える。** `127.0.0.1:8098/metrics` に HAProxy の
  Prometheus exporter を出す。CDN が古い応答を返している間、外から叩いても
  オリジンの停止には気付けない
- **計測基盤も yunirun が持つコンテナ。** Prometheus・Loki・Alloy・Grafana を
  Quadlet で立て、設定も converge が生成する。取り込み対象は HAProxy の
  exporter だけでよく、そこに全アプリの応答が集まっているのでアプリ側に
  計装が要らない。ログは journald を読むので、こちらもアプリ側の変更が要らない
- **秘密の値は unit ファイルに書かない。** unit にはパスだけを置く
- **復号した値はディスクに置く。** 以前は tmpfs に置いていたが、unit が
  `EnvironmentFile=` で参照するため、再起動で消えると起動そのものが失敗した。
  converge との間に順序関係を張れない (アプリはユーザ unit、converge は
  システム unit) ので、順序ではなく依存を消した。age の識別鍵が既に平文で
  ディスク上にある以上、ディスクを読める者は今でも全ての秘密を復号できる
- **ヘルスチェックは Host ヘッダを送る。** `option httpchk` の既定は HTTP/1.0
  かつ Host 無しで、Host で振り分けるアプリはそれを 404 にする。curl は常に
  Host を送るため手元の確認では 200 に見え、原因が分かりにくい
- **生成物は決定的。** 内容が同じなら毎回同じ文字列になる。そうでないと
  converge のたびに unit が書き換わり無用な再起動を招く

## コマンド

| | 実行主体 | 頻度 | 内容 |
|---|---|---|---|
| `yunirun converge` | root | 設定変更時 | 宣言されたアプリ一覧に実体を一致させる |
| `yunirun deploy <sha>` | アプリの deploy ユーザ | push ごと | pull → schema 適用 → blue/green 入替 |

`converge` は冪等な収束操作。`yunirun` 自体が宣言的なので、NixOS の外に出ても
状態が追えなくならない。

schema の適用は provision ではなく deploy 側にある。デプロイのたびに必要なため。

## 計測基盤を見る

すべて 127.0.0.1 にだけ bind してある。外から見るときは ssh のポート転送を使う。

```sh
ssh -N -L 8090:127.0.0.1:8090 yuniruyuni.net
# http://127.0.0.1:8090 を開く
```

Grafana には Prometheus と Loki が最初から繋いである。

- **どの系が落ちているか**: `haproxy_server_status{state="UP"}`
- **応答数と失敗数 (RED の R と E)**: `haproxy_backend_http_responses_total`
- **応答時間 (D)**: `haproxy_backend_response_time_average_seconds`
- **ログ**: `{job="journal"}`。`unit` と `container` で絞り込める

外から HTTP を叩いても健全性の確認にはならない。Cloudflare の
`stale-while-revalidate` により、オリジンが完全に止まっていても 200 が返る
(実測で確認済み)。オリジンの生死はここで見る。

トレースは入れていない。OTLP を吐くアプリがまだ 1 つも無く、置いても空の
UI が増えるだけになる。計装したら Tempo を 1 つ足せば繋がる。
