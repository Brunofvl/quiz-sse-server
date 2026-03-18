# quiz-sse-server

Servidor Go para fazer polling no Supabase REST e distribuir estado do jogo via SSE para clientes React.

## Rodar localmente

```bash
cp .env.example .env
go run .
```

## Build local

```bash
go build -o quiz-sse-server .
```

## Endpoints

- `GET /events`
- `GET /health`

## Deploy na VPS com Docker

```bash
docker build -t quiz-sse-server .
docker run -d --name quiz-sse-server \
  --env-file .env \
  -p 8080:8080 \
  quiz-sse-server
```

## Nginx reverse proxy (SSE)

```nginx
server {
    listen 80;
    server_name seusite.com;

    location /events {
        proxy_pass http://127.0.0.1:8080/events;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Connection "";
        proxy_cache off;
        proxy_buffering off;
        chunked_transfer_encoding off;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```
