FROM golang:1.21-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod download && go build -o quiz-sse-server .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /root/
COPY --from=builder /app/quiz-sse-server .
EXPOSE 8080
CMD ["./quiz-sse-server"]
