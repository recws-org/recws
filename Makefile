install:
	go get

linter-golangci-lint:
	golangci-lint run

linter-ls-lint:
	curl -sL -o ls-lint https://github.com/loeffel-io/ls-lint/releases/download/v2.3.0-beta.3/ls-lint-darwin-arm64 && chmod +x ls-lint && ./ls-lint

linter:
	make linter-ls-lint
	make linter-golangci-lint
