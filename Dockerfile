FROM docker.io/library/golang:1.26.4-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /app/inventory-service ./cmd/main.go

FROM alpine:latest
RUN apk upgrade --no-cache && apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/inventory-service .
EXPOSE 8080

# ENTRYPOINT (not CMD) so the migrate init container/compose can pass the
# `migrate` subcommand via args while the main container serves with no args.
ENTRYPOINT ["./inventory-service"]
