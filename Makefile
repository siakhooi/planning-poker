run:
	go run app/main.go
curl:
	curl http://localhost:8080

golangci-lint:
	golangci-lint run 2>&1 | tee golangci-lint.log

build:
	go mod tidy
	scripts/build.sh 2>&1 | tee build.log

build-all: build docker-build

clean:
	rm -f *.log

docker-build:
	docker build -t siakhooi/planning-poker -f docker/Dockerfile .

docker-run:
	docker run -p 8080:8080 siakhooi/planning-poker
