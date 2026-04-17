.PHONY: build run dev tidy

BIN=./bin/at

build:
	go build -o $(BIN) ./cmd/at

run: build
	$(BIN)

dev:
	go run ./cmd/at

tidy:
	go mod tidy

.DEFAULT_GOAL := build
