# --platform pins the builder to the BUILD host so a multi-arch build
# cross-compiles instead of running this whole stage under emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.6-alpine AS builder
ARG TARGETOS TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build the whole cmd package (not just main.go): cmd/ has sibling files that
# main.go's subcommand dispatch depends on.
RUN CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH}" go build -o /app/inventory-service ./cmd

FROM alpine:3.22
RUN apk upgrade --no-cache && apk --no-cache add ca-certificates \
    && adduser -D -u 65532 app
WORKDIR /home/app/
COPY --from=builder /app/inventory-service .
# Non-root by default — matches the cluster's PSS-restricted runAsNonRoot and
# keeps the image safe when run outside Kubernetes (compose, ad-hoc docker).
USER app
EXPOSE 8080

# ENTRYPOINT (not CMD) so the migrate init container/compose can pass the
# `migrate` subcommand via args while the main container serves with no args.
ENTRYPOINT ["./inventory-service"]
