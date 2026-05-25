FROM golang:1.25 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -o shoes-microsandbox .

FROM debian:bookworm-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/shoes-microsandbox /usr/local/bin/shoes-microsandbox

ENTRYPOINT ["shoes-microsandbox"]