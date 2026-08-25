.PHONY: build test vet fmt run clean

build:
	go build -o bin/eleonesqld ./cmd/eleonesqld
	go build -o bin/eleonesql ./cmd/eleonesql

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -l .

run: build
	./bin/eleonesqld -data eleonesql.edb -wal eleonesql.wal -addr :5432

clean:
	rm -rf bin *.edb *.wal
