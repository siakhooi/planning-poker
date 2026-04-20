run:
	go run app/main.go
curl:
	curl http://localhost:8080

golangci-lint:
	golangci-lint run 2>&1 | tee golangci-lint.log

build:
	go mod tidy
	scripts/build.sh 2>&1 | tee build.log

clean:
	rm -f *.log
