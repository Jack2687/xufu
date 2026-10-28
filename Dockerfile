FROM node:22-bookworm-slim AS ui
WORKDIR /src/frontend
COPY frontend/package*.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-bookworm AS server
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOMAXPROCS=2 go build -p 2 -trimpath -o /xupay ./cmd/server

FROM node:22-bookworm-slim
ARG SOURCE_REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/yigerende/xufu" org.opencontainers.image.revision=$SOURCE_REVISION
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates tzdata curl python3 && rm -rf /var/lib/apt/lists/* && useradd --uid 10001 --create-home xupay
WORKDIR /app
COPY --from=server /xupay /app/xupay
COPY --from=ui /src/webui /app/webui
COPY runtime/ /app/runtime/
ENV PLAYWRIGHT_BROWSERS_PATH=/opt/playwright
RUN npm --prefix runtime ci --omit=dev --no-audit --no-fund && cd runtime && npx playwright install --with-deps chromium && chmod -R a+rX /opt/playwright /app && rm -rf /var/lib/apt/lists/*
USER xupay
ENV LISTEN_HOST=0.0.0.0 PORT=18860 TZ=Asia/Shanghai
EXPOSE 18860
CMD ["/app/xupay"]
