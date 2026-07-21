FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/aetheris ./cmd/gateway

# distroless: kabuk yok, paket yoneticisi yok, nonroot -> saldiri yuzeyi minimum
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/aetheris /aetheris
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/aetheris"]
