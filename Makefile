# 本機開發。容器預設用 podman；用 docker 的話：make up COMPOSE="docker compose -f deploy/compose.yaml"
# 沒有 NVIDIA 顯卡時：make up COMPOSE_FILE=deploy/compose.cpu.yaml
COMPOSE_FILE ?= deploy/compose.yaml
COMPOSE ?= podman compose -f $(COMPOSE_FILE)
CONTAINER ?= podman
export FOOTNOTE_DATABASE_URL ?= postgres://footnote:footnote@127.0.0.1:25432/footnote
TEST_DATABASE_URL ?= postgres://footnote:footnote@127.0.0.1:25432/footnote_test
EVAL ?= eval/eval.example.yaml

.PHONY: up down wait build test migrate ingest serve eval

up:        ## 起 Postgres + 兩個 TEI（第一次會下載約 5GB 模型）
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

wait:      ## 等 Postgres 與兩個 TEI 都就緒（第一次要等模型下載完）
	@until $(CONTAINER) exec footnote-db-1 pg_isready -U footnote -q; do sleep 2; done
	@echo "等 TEI 載入模型…"; until curl -sf http://127.0.0.1:28080/health >/dev/null && curl -sf http://127.0.0.1:28081/health >/dev/null; do sleep 5; done
	@echo "就緒"

build:
	go build -o bin/footnote ./cmd/footnote

# 沒有 footnote.yaml 時從範本複製（已經有就不動）
footnote.yaml:
	cp footnote.example.yaml $@

test:      ## commit 前跑；整合測試會清空 footnote_test 資料庫
	go vet ./... && FOOTNOTE_TEST_DATABASE_URL=$(TEST_DATABASE_URL) go test -count=1 -p 1 ./...  # -p 1：整合測試共用 footnote_test，不能平行

migrate: build footnote.yaml
	./bin/footnote migrate
	-$(CONTAINER) exec footnote-db-1 psql -U footnote -tc "SELECT 1 FROM pg_database WHERE datname='footnote_test'" | grep -q 1 || $(CONTAINER) exec footnote-db-1 createdb -U footnote footnote_test

ingest: migrate
	./bin/footnote ingest

serve: build footnote.yaml
	./bin/footnote serve

eval: build footnote.yaml
	./bin/footnote eval -f $(EVAL) --sweep
