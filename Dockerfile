FROM golang:1.25-alpine AS builder

WORKDIR /app

RUN apk add --no-cache ca-certificates git tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/mumu-bot .

FROM alpine:3.22

WORKDIR /app

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/mumu-bot /app/mumu-bot
COPY --from=builder /app/config /app/config

RUN mkdir -p /app/stickers

EXPOSE 8080

ENTRYPOINT ["/app/mumu-bot"]
