BINARY_NAME := webex-mcp
BUILD_DIR := bin
CMD_DIR := cmd/webex-mcp
GOLANGCI_LINT := golangci-lint

.PHONY: build test lint fmt clean

build:
	go build -o $(BUILD_DIR)/$(BINARY_NAME) ./$(CMD_DIR)/

test:
ifdef T
	go test -v -run '$(T)' ./...
else
	go test -v ./...
endif

lint:
	$(GOLANGCI_LINT) run ./...

fmt:
	gofmt -w .
	goimports -w .

clean:
	rm -rf $(BUILD_DIR)
