FROM golang:1.26-alpine AS builder
WORKDIR /app
COPY . .
RUN go build -C app -o /app/server

FROM scratch
WORKDIR /
COPY --from=builder /app/server /
CMD ["/server"]
