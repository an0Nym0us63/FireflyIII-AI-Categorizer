FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod ./
RUN go mod tidy
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /firefly-ai-categorize ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /data
WORKDIR /app
COPY --from=builder /firefly-ai-categorize .
COPY public/ public/
ENV CONFIG_FILE=/data/config.json
VOLUME /data
EXPOSE 3000
CMD ["/app/firefly-ai-categorize"]
