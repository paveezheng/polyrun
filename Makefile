APP_NAME := polyrun
GOEXE := $(shell go env GOEXE)
BIN := $(APP_NAME)$(GOEXE)

.PHONY: build

build:
	go build -trimpath -o $(BIN) .
