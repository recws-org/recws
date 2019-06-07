install:
	go get -t -v -u ./...

linter:
	golangci-lint run --enable-all

test:
	make linter