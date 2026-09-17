# msnw — my small network

名前でサービスを公開して、NAT の向こうから P2P で繋ぎに行く小さなトンネル。

```
        ┌──────────────┐  紹介 / NAT 越え補助 / 最後の手段のリレー
        │ msnw-server  │  (QUIC 制御 :4433, UDP リレー :4434)
        └──────┬───────┘
     登録 ↗           ↖ 問い合わせ
┌──────────────┐  QUIC (P2P, 直結 or リレー)  ┌──────────────┐
│ msnw-exporter│ ◀═══════════════════════════▶ │ msnw-client  │
│  -n hogehoge │   1 TCP 接続 = 1 QUIC stream  │              │
│  -t 1234     │                                │ stdio / -l / │
└──────┬───────┘                                │   --socks5   │
       ▼                                        └──────────────┘
   nc -l 1234
```

## ビルド

```
make            # CGO_ENABLED=0 の静的バイナリを bin/ に生成
make cross      # linux/darwin/windows 向けを bin/<os>-<arch>/ に生成
```

Go 1.26 以上(quic-go の要件)。`go build` を直接使うときは `CGO_ENABLED=0` を付けること。
cgo 付きでビルドすると libc を動的リンクし、古い glibc のホストで
`GLIBC_2.34' not found` のようなエラーになる。

## 使い方

全コンポーネント共通で `-secret`(または `$MSNW_SECRET`)が必要。
exporter / client は `-s host:port`(または `$MSNW_SERVER`、既定 `localhost:4433`)でサーバーを指す。

### server

```
msnw-server -secret S [-listen :4433] [-relay :4434] [-key server.key]
```

UDP の 2 ポートを外から到達可能にしておく。`-key` を指定すると鍵を保存して
フィンガープリントが再起動をまたいで固定される。起動時に表示される
`fingerprint: sha256:...` を exporter / client の `-server-fp`(または
`$MSNW_SERVER_FP`)に渡すとサーバーをピン留めできる。

### exporter

```
msnw-exporter -n hogehoge -t 1234                    # localhost:1234 を hogehoge として公開
msnw-exporter -n hogehoge -t 1234 -t 8080 -t db:5432 # 複数ターゲット。最初のものが既定
msnw-exporter -n exit --all                          # 任意の host:port へ中継(exit node 的用途)
```

`-t` は `port`(= localhost:port)または `host:port`。`--all` は `-t` と併用でき、
その場合 `-t` の先頭が既定ターゲットになる。

### client

```
msnw-client -n hogehoge                 # stdin/stdout をそのまま繋ぐ(nc / ssh ProxyCommand 用)
msnw-client -n hogehoge:8080            # exporter 側の別ポートを指定
msnw-client -n exit:example.com:80      # --all な exporter 経由で任意ホストへ

msnw-client -n hogehoge -l              # exporter の既定ポートと同じ番号で 127.0.0.1 に listen
msnw-client -n hogehoge -l 5000         # 127.0.0.1:5000 → hogehoge の既定ターゲット
msnw-client -n hogehoge:8080 -l :5000   # 全インターフェイスで listen

msnw-client --socks5                    # 127.0.0.1:1080 で SOCKS5
msnw-client --socks5 :1080 -n exit      # 不明なホストは exit 経由で外へ
```

SOCKS5 モードでの宛先ホストの解釈:

| 宛先ホスト               | 行き先                                    |
|--------------------------|-------------------------------------------|
| `hogehoge`               | exporter hogehoge の、宛先ポート           |
| `hogehoge.msnw`          | 同上                                       |
| `db.hogehoge.msnw`       | exporter hogehoge から `db:<port>` へ      |
| それ以外(FQDN / IP)    | `-n` で指定した既定 exporter から外へ      |

`.msnw` 形式を使うときは `curl --socks5-hostname` / `ssh -o ProxyCommand='nc -X 5 -x ... %h %p'` のように
名前解決をプロキシに任せる設定にする。

例: ssh

```
ssh -o ProxyCommand='msnw-client -n hogehoge:22' user@anything
```

## 仕組み

- **1 ソケット共用**: 各ノードは 1 つの UDP ソケットで server との制御 QUIC 接続と
  peer との QUIC 接続を両方さばく。server が制御接続で観測した「外から見えるアドレス」が
  そのまま P2P で使う NAT マッピングになる(STUN 相当)。
- **紹介**: client が `-n` の名前を問い合わせると、server は exporter に client の候補
  アドレス(反射アドレス + LAN アドレス)を、client に exporter の候補を渡す。
- **ホールパンチ**: 両側が相手の全候補へ小さな UDP パケットを撃ちつつ、client が
  全候補へ並行して QUIC を dial する。最初に握手が終わったものを採用。
- **リレー**: 1.5 秒経っても直結できなければ server のリレーポート経由で QUIC を張る。
  リレーは UDP をそのまま転送するだけなので、暗号化は end-to-end のまま。
  対称 NAT 同士などはここに落ちる。
- **認証**: server への認証は共有シークレットを TLS の keying material に HMAC で
  束縛したもの(再送攻撃不可)。peer 同士は server から受け取った相手の公開鍵
  フィンガープリントをピン留めして相互 TLS 認証する。exporter は server が紹介した
  client しか受け付けない。

## 注意

- 既定では server 証明書を検証しない(共有シークレットで server には認証されるが、
  server のなりすましは防げない)。真面目に運用するなら `-key` で鍵を固定し、
  `-server-fp` でピン留めする。
- Linux で UDP 受信バッファが小さいと quic-go が警告する。高スループットが要るなら
  `sysctl -w net.core.rmem_max=7500000 net.core.wmem_max=7500000`。
- デバッグ用に `MSNW_FORCE_RELAY=1` で client を直結せずリレーのみにできる。
  `-v` で dial の失敗理由を表示。
