FROM golang:1.21-alpine AS builder
RUN apk add --no-cache gcc musl-dev
WORKDIR /app
COPY . .
RUN go mod download && \
    go build -o bot . && \
    go build -o broadcast ./cmd/broadcast

FROM alpine:latest
RUN apk --no-cache add ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/bot .
COPY --from=builder /app/broadcast .
CMD ["./bot"]
