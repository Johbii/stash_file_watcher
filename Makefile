.DEFAULT_GOAL := build

main := ./main.go
binary := ./stash_file_watcher
main_dir := $(shell dirname $(main))
go_files := $(shell find . -type f -name "*.go")

.PHONY: audit
audit:
	go mod tidy -diff
	go mod verify
	test -z "$(shell gofmt -l .)"
	go vet $(go_files)
	go run honnef.co/go/tools/cmd/staticcheck@latest -checks=all,-S1000,-U1000 $(go_files)
	go run golang.org/x/vuln/cmd/govulncheck@latest .

.PHONY: tidy
tidy:
	go mod tidy -v
	go fmt $(go_files)

.PHONY: build
build: $(binary)

$(binary): $(go_files)
	go mod tidy -v
	go fmt $(go_files)
	go build $(main_dir)

.PHONY: clean
clean:
	rm -f ./stash_file_watcher
