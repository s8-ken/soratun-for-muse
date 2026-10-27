# 上流との差分一覧

ベースバージョン:

- soratun: **v1.2.8**（タグ `v1.2.8`、https://github.com/soracom/soratun）
- wireguard-go: **v0.0.0-20231211153847-12269c276173**（soratun v1.2.8 の go.mod が要求する版）

機械可読な差分は `patches/` 以下に .patch 形式で同梱しています。

- `patches/wireguard-go-conn.patch`（wireguard-go `conn` パッケージ全体の差分）
- `patches/soratun-go.mod.patch`（soratun `go.mod` の差分）

## 1. soratun 側

| ファイル | 変更 |
|---|---|
| `third_party/soratun/go.mod` | 末尾に `replace golang.zx2c4.com/wireguard => ../wireguard-go` を追加し、パッチ版 wireguard-go を使うよう変更。これ以外に本体コードの変更はなし |

## 2. wireguard-go 側

差分ファイルは 2 つだけです（`conn/bind_std.go` の変更と `conn/bind_rotate.go` の新規追加）。
それ以外のファイルは上流とバイト単位で同一であることを diff で確認済みです。

### 2.1 `conn/bind_std.go`（変更）

| 箇所 | 変更内容 |
|---|---|
| `StdNetBind` 構造体 | connect 済み宛先を保持するフィールド `connected4` / `connected6` を追加 |
| `ensureConnected()`（新規メソッド） | 送信前に `unix.Connect` でソケットを宛先へ接続する。既に同じ宛先へ接続済みなら何もしない |
| `Send()`（Linux パス） | パケットごとの宛先指定（sendmmsg）をやめ、`ensureConnected` 後に通常の `Write` で送信。IPv4-in-IPv6 エンドポイントは通常の IPv4 へ正規化してから処理 |
| `Receive` 系（受信時のエンドポイント決定） | 受信パケットの送信元でエンドポイントを上書き（ローミング）しないよう変更。接続済みソケットでは宛先は connect 先に固定される |
| `Close()` / 再 Open | 接続状態（connected4/6）をリセットするよう変更 |
| `NewStdNetBind()` | 返却する Bind を `NewRotateBind()` に差し替え（下記 2.2 を既定化） |

### 2.2 `conn/bind_rotate.go`（新規追加）

`RotateBind`: 「1 フローあたり受信 1 パケットまで」という中継環境向けの Bind 実装。

- 送信パケットごとに新しい接続済み UDP ソケット（`DialUDP`）を作成して送信
- 受信はソケットごとの reader goroutine が共有チャネルへ集約
- サーバー側 WireGuard の標準ローミングにより、返信は常に最新ソケットへ届くため、
  各ソケットの「未使用の 1 パケット予算」で受信できる
- ソケットの寿命管理: ハードキャップ 120 秒、後継ソケット出現後 45 秒の猶予で回収、
  受信済みソケットは即 close
- `Open` 時に内部状態をリセット（UAPI のポート再設定で Close→再 Open されるため必須）
- 受信パケットのエンドポイントは、設定済みサーバーエンドポイントに固定（2.1 のローミング無効化と同方針）

## 3. パッチの再現方法

このリポジトリの `third_party/` はパッチ適用済みですが、上流から再現する場合は:

パッチファイル内のパスは本リポジトリの構成（`third_party/...`）に合わせています。
上流ソースを同じ構成で配置すれば、リポジトリルートから適用できます（適用後に
`conn` がパッチ版と一致することを確認済み）。

```sh
# 上流 soratun v1.2.8 を third_party/soratun、上流 wireguard-go を
# third_party/wireguard-go に配置した状態で、リポジトリルートから:
git apply patches/wireguard-go-conn.patch
git apply patches/soratun-go.mod.patch
```
