FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
COPY internal/ ./internal/
COPY cmd/ ./cmd/
COPY web/ ./web/
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /go-frog . && \
    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /go-frog-audit ./cmd/audit

FROM debian:bookworm-slim AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*
COPY --from=build /go-frog /go-frog
COPY --from=build /go-frog-audit /go-frog-audit
EXPOSE 8080
ENTRYPOINT ["/go-frog"]
