# Local development. For production, build the API via Dockerfile.api.

.PHONY: build sync-embed cli api docker clean

BIN_DIR := bin

sync-embed:
	@mkdir -p cmd/api/skill cmd/api/dist
	@cp skills/preview/SKILL.md cmd/api/skill/SKILL.md
	@[ -e cmd/api/dist/.keep ] || touch cmd/api/dist/.keep

cli:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/preview ./cmd/preview

api: sync-embed
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/preview-api ./cmd/api

build: cli api

docker:
	docker build -f Dockerfile.api -t preview-api:local .

clean:
	rm -rf $(BIN_DIR) cmd/api/skill cmd/api/dist/preview-*
