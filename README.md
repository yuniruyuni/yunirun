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

### 1. システム側 (`/etc/yunirun/config.json`)

取り込みの意思決定だけ。NixOS モジュールが書き出す。

```json
{
  "domain": "yuniruyuni.net",
  "apps": { "fighter": "yuniruyuni/fighter-notes" }
}
```

これがそのまま opkssh の認可リストと対応する。**アプリ側が自分を勝手に
取り込ませることはできない。**

### 2. yunirun が導出するもの

uid/gid、ホストポート、unit 名、HAProxy backend 名、DB 名、ロール名、
そして DB パスワード。

**アプリが知る必要も宣言する必要もない、純粋なホスト側の事情。**
これらを人が書くと番号を重複させる。実際、以前この計算を手書きしていた時期に
uid を既存ユーザと衝突させ、あるアプリの deploy ユーザが別アプリの DB
パスワードを読める状態を作ったことがある。`internal/alloc` に閉じ込めて
名前から一意に導出し、人が触れないようにしてある。

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
    "cleanup": { "schedule": "02:23", "args": ["--batch=cleanup"] }
  }
}
```

秘密は `secrets/<ENV_NAME>.age` というファイル名自体が宣言を兼ねるので、
ここに列挙しない。

**マニフェストはデプロイ時に運ばれる。** VPS が GitHub を取りに行かないので、
private リポジトリでも認証情報を置かずに済む。

## 設計上の決めごと

- **コンテナは 127.0.0.1 にのみ publish する。** 外部からの到達は Cloudflare
  Tunnel 経由に限る
- **PostgreSQL へは Unix ソケットで繋ぐ。** コンテナは独立した netns に置き、
  TCP を使わないことでホストの loopback 上の他サービスへ到達できないようにする
- **秘密の値は unit ファイルにもディスクにも書かない。** podman secret の
  参照だけを置き、実行時に取得する
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
