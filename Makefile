run:
	go run ./app
curl:
	curl http://localhost:8080

golangci-lint:
	golangci-lint run 2>&1 | tee golangci-lint.log
test:
	./scripts/test.sh
build:
	go mod tidy
	scripts/build.sh 2>&1 | tee build.log

all: build docker-build

release:
	scripts/create-release.sh

clean:
	rm -f *.log

docker-build:
	docker build -t siakhooi/fibo-planner -f docker/Dockerfile .

docker-run:
	docker run -p 8080:8080 siakhooi/fibo-planner

curl-ws:
	curl -sS -N  ws://localhost:8080/ws

websocat-ws:
	 websocat ws://localhost:8080/ws
