FROM golang:1.24-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git
COPY . .
RUN go mod tidy
ARG VERSION=dev
ARG COMMIT=
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/openaccountants/firefly-iii-ai-categorize/internal/version.Version=${VERSION} -X github.com/openaccountants/firefly-iii-ai-categorize/internal/version.Commit=${COMMIT}" \
    -o /firefly-ai-categorize ./cmd/server

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /data
WORKDIR /app
COPY --from=builder /firefly-ai-categorize .
COPY public/ public/
ENV CONFIG_FILE=/data/config.json
VOLUME /data
EXPOSE 3000
CMD ["/app/firefly-ai-categorize"]
