# syntax=docker/dockerfile:1

FROM python:3.12-slim

LABEL org.opencontainers.image.source="https://github.com/wangxiangg1/vps-tool"

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PYTHONPATH=/app/server \
    VPS_TOOL_DB_PATH=/data/vps-tool.sqlite3

WORKDIR /app

RUN addgroup --system app \
    && adduser --system --ingroup app app

COPY server/requirements.txt /app/server/requirements.txt
RUN python -m pip install --no-cache-dir -r /app/server/requirements.txt

COPY server /app/server
COPY web /app/web

RUN mkdir -p /data \
    && chown -R app:app /app /data

USER app

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD ["python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/api/health', timeout=3)"]

STOPSIGNAL SIGTERM

CMD ["python", "-m", "uvicorn", "app.main:app", "--app-dir", "/app/server", "--host", "0.0.0.0", "--port", "8000", "--workers", "1"]
