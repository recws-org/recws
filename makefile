install:
	go get

linter:
	golangci-lint run

test:
	make linter