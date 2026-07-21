FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/aetheris ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/aetheris /aetheris
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/aetheris"]
