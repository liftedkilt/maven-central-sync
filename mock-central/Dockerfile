FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /mock-central .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
RUN adduser -D -u 1000 appuser
COPY --from=build /mock-central /mock-central
USER appuser
EXPOSE 8082
ENTRYPOINT ["/mock-central"]
