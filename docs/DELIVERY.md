# AdaGrade 建置流程

# 建置
## 1. 安裝需求

- Go 1.26+
- Node.js 20+ 與 npm
- Docker
- C compiler

## 2. 設定環境

```sh
# 確認在工作區
cp .env.adamarker.example .env
ADAMARKER_BOOTSTRAP_ADMIN_EMAIL=you@example.edu
```

## 3. 建置並啟動

```sh
make frontend
make build
```

```sh
# 系統必須由人啟動
sudo make dev
```

開啟 <http://localhost:8080>。

## 4. 驗證

```sh
# 系統無法運行時，由人跑並且診斷
make test
make test-integration
```

## 5. 停止資料庫

```sh
make db-down
```

## 6. 清理

```sh
make db-down
docker compose down --volumes --remove-orphans
sudo rm -rf bin .cache internal/web/assets/dist
```

# 使用

## 本機開發登入

1. 以 `make dev` 啟動服務，然後開啟 <http://localhost:8080/login>。
2. 使用: you@example.edu 登入

## 上傳


## 下載 latex
