# shoes-microsandbox

[myshoes](https://github.com/whywaita/myshoes) のプロバイダーで、[microsandbox](https://github.com/superradcompany/microsandbox) を使って軽量なハードウェア分離microVMを起動し、GitHub Actions self-hosted runnerとして利用します。

`AddInstance` が呼ばれるたびに新しいmicrosandbox VMを起動し、VM内でGitHub Actions runnerのセットアップスクリプトを実行してIPアドレスを返します。ジョブが完了すると `DeleteInstance` がVMを停止・削除し、状態を完全にクリアします。

## アーキテクチャ

```
┌─────────────────────────────────────────────┐
│                  myshoes                     │
│            (オーケストレータ / スケジューラ)  │
└──────────────────┬──────────────────────────┘
                   │ gRPC (hashicorp go-plugin)
                   │ AddInstance / DeleteInstance
┌──────────────────▼──────────────────────────┐
│         shoes-microsandbox                    │
│   ┌─────────────────────────────────────┐    │
│   │  AddInstance:                       │    │
│   │    1. CreateSandbox(image, mem, cpu)│    │
│   │    2. Exec(setup_script)            │    │
│   │    3. Detach (VMはバックグラウンドで実行) │    │
│   │    4. cloud_id + IP を返す           │    │
│   │                                     │    │
│   │  DeleteInstance:                    │    │
│   │    1. GetSandbox(cloud_id)          │    │
│   │    2. Stop → Remove                 │    │
│   └─────────────────────────────────────┘    │
└──────────────────┬──────────────────────────┘
                   │ microsandbox SDK (組み込みFFI)
                   │
┌──────────────────▼──────────────────────────┐
│           microsandbox ランタイム             │
│     (libkrunfw + msb, 自動ダウンロード)       │
└──────────────────┬──────────────────────────┘
                   │ microVM (KVM / Apple Silicon)
┌──────────────────▼──────────────────────────┐
│        GitHub Actions Runner                 │
│    (エフェメラルVM — ジョブごとに分離)        │
└───────────────────────────────────────────────┘
```

## 前提条件

- **Linux** でKVMが有効であること（`/dev/kvm`が存在）、**または** Apple Silicon搭載のmacOS
- Go 1.24+
- `msb` ランタイムと `libkrunfw` は初回起動時に `microsandbox.EnsureInstalled()` で自動ダウンロードされます

## クイックスタート

### デフォルトイメージで実行

shoes-microsandboxは、公式の[GitHub Actions Runnerコンテナイメージ](https://github.com/actions/runner/pkgs/container/actions-runner)（`ghcr.io/actions/actions-runner`）をデフォルトのVMイメージとして使用します。このイメージにはrunnerエージェントとGitHub Actionsに接続するための依存関係がすべて含まれています。

```bash
# プラグインをビルド
go build -o shoes-microsandbox .

# myshoesの環境変数を設定
export SHOES_PLUGIN_PATH=/path/to/shoes-microsandbox

# （オプション）特定のrunnerバージョンを固定
export MSB_IMAGE=ghcr.io/actions/actions-runner:2.334.0

# myshoesを起動
myshoes server
```

### カスタムイメージの使用

任意のOCI互換イメージをrunner VMとして使用できます。プライベートレジストリの場合は認証情報を設定します：

```bash
# GHCRのプライベートイメージ
export MSB_IMAGE=ghcr.io/myorg/custom-runner:latest
export MSB_REGISTRY_USERNAME=myuser
export MSB_REGISTRY_PASSWORD=ghp_xxxxxxxxxxxx

# または Docker Hub
export MSB_IMAGE=docker.io/myorg/runner:latest
export MSB_REGISTRY_USERNAME=myuser
export MSB_REGISTRY_PASSWORD=dckr_pat_xxxxxxxxxxxx
```

### Runnerバージョン

`ghcr.io/actions/actions-runner` イメージはrunnerバージョンごとにタグ付けされています。主なタグ：

| タグ | 説明 |
|---|---|
| `latest` | 最新の安定版runnerリリース |
| `2.334.0` | 特定のrunnerバージョン |
| `2.333.1` | 1つ前のrunnerバージョン |

バージョンを固定して予期しない更新を防ぐことができます：

```bash
export MSB_IMAGE=ghcr.io/actions/actions-runner:2.334.0
```

## 設定

すべて環境変数で設定します：

| 変数 | デフォルト | 説明 |
|---|---|---|
| `MSB_IMAGE` | `ghcr.io/actions/actions-runner:latest` | VM用OCIイメージ |
| `MSB_MEMORY_MIB` | `2048` | サンドボックスあたりのメモリ (MiB) |
| `MSB_CPUS` | `2` | サンドボックスあたりのCPU数 |
| `MSB_RUNNER_USER` | `runner` | VM内での実行ユーザー |
| `MSB_REGISTRY_USERNAME` | *(空)* | プライベートレジストリのユーザー名 |
| `MSB_REGISTRY_PASSWORD` | *(空)* | プライベートレジストリのパスワード |
| `MSB_PULL_POLICY` | `""` (if-missing) | イメージPullポリシー: `always`, `if-missing`, `never` |
| `SHOES_PLUGIN_MAGIC_COOKIE` | *(myshoesが設定)* | プラグインハンドシェイクCookie |
| `SHOES_PLUGIN_PATH` | *(myshoesが設定)* | このバイナリのパス |

### リソースタイプマッピング

myshoesの `ResourceType` 値は以下のようにサンドボックスリソースにマッピングされます：

| ResourceType | メモリ (MiB) | CPUs |
|---|---|---|
| Nano | 512 | 1 |
| Micro | 1024 | 1 |
| Small | 2048 | 2 |
| Medium | 4096 | 2 |
| Large | 8192 | 4 |
| XLarge | 16384 | 4 |
| XLarge2 | 32768 | 8 |
| XLarge3 | 65536 | 8 |
| XLarge4 | 131072 | 16 |

## ビルド

```bash
go build -o shoes-microsandbox .
```

## myshoesでの使用方法

1. バイナリをビルド：
   ```bash
   go build -o shoes-microsandbox .
   chmod +x shoes-microsandbox
   ```

2. myshoesにこのプラグインを使用するよう設定：
   ```bash
   export SHOES_PLUGIN_PATH=/path/to/shoes-microsandbox
   ```
   またはURLを指定：
   ```bash
   export SHOES_PLUGIN_PATH=https://releases.example.com/shoes-microsandbox
   ```

3. myshoesサーバーを起動：
   ```bash
   myshoes server
   ```

4. myshoes APIでターゲットを作成し、GitHubリポジトリ/組織を指定。

## Docker

```dockerfile
FROM golang:1.24 AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o shoes-microsandbox .

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
COPY --from=builder /app/shoes-microsandbox /usr/local/bin/shoes-microsandbox
ENTRYPOINT ["shoes-microsandbox"]
```

## 動作原理

### AddInstance

1. myshoesから `runner_name` と `setup_script` を受信
2. `ResourceType` からサンドボックスリソースを計算
3. `CreateSandbox()` でmicrosandbox VMを作成：
   - 設定されたOCIイメージを使用（デフォルト: `ghcr.io/actions/actions-runner:latest`）
   - 認証情報が設定されていれば適用
   - Pullポリシーを適用（デフォルト: `if-missing`）
   - ホスト名を `myshoes-<runner_name>` に設定
   - `WithReplace()` で既存サンドボックスとの競合を回避
4. VM内でGitHub Actions runnerセットアップスクリプトを実行
5. VMのIPアドレスを取得
6. VMからデタッチ（VMは独立して実行を継続）
7. `cloud_id`, `ip_address`, `shoes_type="shoes-microsandbox"` を返す

### DeleteInstance

1. `cloud_id` でサンドボックスを検索
2. VMをグレースフルストップ（失敗時はキル）
3. サンドボックスの永続化状態を削除
4. VMは完全に消滅 — リソースの残留なし

## セキュリティ

- 各GitHub Actionsジョブは**ハードウェア分離されたmicroVM**で実行（共有コンテナではない）
- ジョブ完了後にVMが破棄され、クリーンな状態が保証される
- microsandboxのシークレット注入とネットワークポリシー機能で追加のセキュリティ強化が可能
- ルートデーモン不要 — SDKがランタイムを直接組み込み

## 制限事項

- **LinuxではKVMが必須** — `/dev/kvm` がない環境では動作しない
- 現在 Linux (x86_64/ARM64) と macOS (Apple Silicon) のみ対応
- `ip_address` はmicrosandboxのネットワーク設定により内部NATアドレスになる場合がある
- ベータソフトウェア — microsandbox v0.xでは破壊的変更が予定されている

## ライセンス

Apache-2.0