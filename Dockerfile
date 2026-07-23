FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/aetheris ./cmd/gateway
RUN mkdir -p /wal

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/aetheris /aetheris
COPY --from=builder --chown=65532:65532 /wal /wal
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/aetheris"]
