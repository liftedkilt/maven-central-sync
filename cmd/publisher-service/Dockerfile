FROM golang:1.23-alpine AS build
WORKDIR /src
# Copy the shared library first
COPY internal/sync/ /src/internal/sync/
# Copy the publisher service
COPY cmd/publisher-service/ /src/cmd/publisher-service/
WORKDIR /src/cmd/publisher-service
RUN CGO_ENABLED=0 go build -o /publisher-service .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser
COPY --from=build /publisher-service /publisher-service
USER appuser
EXPOSE 8080
ENTRYPOINT ["/publisher-service"]
