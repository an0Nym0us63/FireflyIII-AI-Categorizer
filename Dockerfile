# The Go binary is compiled on the CI runner (see .github/workflows/docker.yml)
# and copied in here — this image only packages it.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && mkdir -p /data
WORKDIR /app
COPY firefly-ai-categorize .
COPY public/ public/
ENV CONFIG_FILE=/data/config.json
VOLUME /data
EXPOSE 3000
CMD ["/app/firefly-ai-categorize"]
