build:
	go build -trimpath -ldflags="-s -w" -o bin/aetheris ./cmd/gateway

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

vet:
	go vet ./...

cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html

run: build
	./bin/aetheris

docker:
	docker build -t aetheris-gateway:latest .

clean:
	rm -rf bin coverage.out coverage.html
