# soratun-patched — 制限付きネットワーク環境向け SORACOM Arc クライアント

AI エージェント用サンドボックス VM のような、**通常とは異なるネットワーク制限が課された環境**でも
[SORACOM Arc](https://soracom.jp/services/arc/)（WireGuard ベースの仮想 SIM 接続サービス）を利用できるように、
公式クライアント [soratun](https://github.com/soracom/soratun) v1.2.8 と、その依存ライブラリ
wireguard-go の `conn` パッケージにパッチを当てた非公式フォークです。

> **注意**: これは SORACOM 公式のソフトウェアではありません。通常の Linux 環境では
> パッチなしの公式 soratun を使ってください。本パッチは下記「対象環境」のような特殊な
> 制約を持つ環境でのみ意味があります。

## なぜ必要だったか

対象環境（Meta Muse のエージェント VM）では、調査の結果、次のようなネットワーク挙動が確認されました。

1. **connect() していない UDP ソケットからの送信が常に EPERM で拒否される**
   （`sendto` / `sendmmsg` でパケットごとに宛先を指定する方式は宛先を問わず失敗する）。
   公式 soratun が使う wireguard-go の標準 Bind はまさにこの方式のため、ハンドシェイクすら送れません。
2. **応答パケットの送信元アドレスがゲートウェイ/プロキシのアドレスに書き換わって届く**ため、
   WireGuard 標準のエンドポイント・ローミングがそれを新しい宛先として採用してしまい、通信が壊れます。
3. **1 つの接続済み UDP ソケット（フロー）で受信できるのは最初の 1 パケットだけ**で、
   2 パケット目以降は間隔を問わず一切届きません。そのため通常の Bind では
   ハンドシェイクが通っても、約 2 分後の鍵更新（リキー）でトンネルが死にます。

これらは環境側のポリシー（直接ネットワークプロトコルの既定 Deny → ユーザーが Ask に切替）や
egress ゲートウェイの仕様によるもので、SORACOM Arc サーバー側の問題ではありません。

## 上流との差分（3点）

詳細は [PATCHES.md](PATCHES.md) と `patches/` 以下の .patch ファイルを参照してください。

### 1. Linux Send の connect() + Write 化（`conn/bind_std.go`）

Linux では、送信前にソケットへ `connect()` を発行し、パケットごとの宛先指定をやめて
単純な `Write` で送るように変更しました。あわせて IPv4-in-IPv6 形式のエンドポイントを
通常の IPv4 に正規化しています。

### 2. エンドポイント・ローミングの無効化（`conn/bind_std.go`）

受信パケットの送信元アドレスでピアのエンドポイントを上書きしないよう変更しました。
接続済みソケットではエンドポイントは接続先に固定されます。

### 3. RotateBind: 送信ごとのソケット回転（`conn/bind_rotate.go`、新規）

`NewStdNetBind` を差し替え、既定の Bind を `RotateBind` にしました。
送信パケットごとに新しい接続済み UDP ソケットを作成し、受信は各ソケットの
reader goroutine が共有チャネルへ流します。サーバー側は標準のローミングで常に
最新の送信元へ返信するため、各フローの「1 パケット予算」を毎回未使用のソケットで
受けられます。旧ソケットは 45 秒の猶予後に回収し、受信済みソケットは即 close します。
（検証: 約 2 分ごとの鍵更新が 8 回連続成功。パッチ前は鍵更新成功ゼロでした。）

soratun 本体側の変更は `go.mod` に wireguard-go への replace を 1 行追加したのみで、
その他のコードは上流 v1.2.8 のままです。

## リポジトリ構成

```
.
├── README.md / PATCHES.md / LICENSE
├── patches/
│   ├── wireguard-go-conn.patch   # 上流 wireguard-go conn パッケージとの差分
│   └── soratun-go.mod.patch      # soratun go.mod の replace 追加差分
├── examples/
│   └── arc.json.example          # ダミー値のサンプル設定
└── third_party/
    ├── soratun/                  # soratun v1.2.8 + go.mod replace
    └── wireguard-go/             # パッチ適用済み wireguard-go
                                  # (v0.0.0-20231211153847-12269c276173 ベース)
```

## ビルド手順

前提: Go 1.23 以降（開発時は Go 1.27.3 で確認）、Linux (amd64)。

```sh
cd third_party/soratun
go build -trimpath -o soratun ./cmd/soratun
# または上流の Makefile どおり: make build
```

`third_party/soratun/go.mod` の replace により、依存する wireguard-go は
自動的に `../wireguard-go`（パッチ版）が使われます。

## 使い方

設定ファイル（`arc.json`）の書式は **soratun 本家と完全に同一** です。
本家 README の手順どおり、SORACOM ユーザーコンソールまたは `soratun bootstrap` /
`soratun config` で発行された設定を使ってください。

```sh
sudo ./soratun up --config ./arc.json
sudo ./soratun down --config ./arc.json
```

サンプル（値はすべてダミー）: [`examples/arc.json.example`](examples/arc.json.example)

補足:
- Arc のエンドポイントは単一宛先のため、connect() 方式でも実用上の問題はありません
- ローミング無効化は「宛先が固定の VPN クライアント」という Arc の性質に依存しています。
  宛先が本当に移動するモバイル用途には向きません

## 対象環境と制約

- 対象: 上記のような UDP 送信方式・中継挙動を持つサンドボックス/ゲートウェイ配下の Linux 環境
- Linux 以外では RotateBind は動作しません（パッチは Linux 向けです）
- TUN デバイスを作成できる権限（root または CAP_NET_ADMIN）が必要です
- トンネル内のデータ面はベストエフォートです。各フローで受信できるのが
  先頭 1 パケットである以上、サーバーから連続送信される通信は TCP の再送等に
  依存します。すべての用途で実用になるとは限りません
- 環境側のポリシー変更やゲートウェイの仕様変更により、将来動作しなくなる可能性があります

## ライセンスとクレジット

本リポジトリ全体は MIT License で公開します（[LICENSE](LICENSE)）。
上流プロジェクトの著作権表示とライセンス文は各ディレクトリにそのまま保持しています。

- **soratun** — Copyright (c) 2021- Soracom, Inc.（MIT License）
  https://github.com/soracom/soratun — `third_party/soratun/LICENSE`
- **wireguard-go** — Copyright (C) 2017-2023 WireGuard LLC（MIT License）
  https://git.zx2c4.com/wireguard-go — `third_party/wireguard-go/LICENSE`

WireGuard は WireGuard LLC の登録商標です。本プロジェクトは SORACOM, Inc. および
WireGuard LLC とは関係のない、個人による非公式の改造版です。

## 免責

本ソフトウェアは「現状のまま」提供され、いかなる保証もありません。
使用は自己責任でお願いします。SORACOM Arc の利用には SORACOM の利用規約が適用されます。
