FROM golang:1.23-alpine AS build
WORKDIR /src
# Copy shared library first
COPY internal/sync/ ./internal/sync/
# Copy sync-service
COPY sync-service/go.mod sync-service/go.sum* ./sync-service/
COPY sync-service/*.go ./sync-service/
WORKDIR /src/sync-service
RUN CGO_ENABLED=0 go build -o /sync-service .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser
COPY --from=build /sync-service /sync-service
USER appuser
EXPOSE 8080
ENTRYPOINT ["/sync-service"]
