FROM python:3.12-slim AS builder
WORKDIR /app
COPY pyproject.toml .
COPY core/ ./core/
COPY providers/ ./providers/
COPY api/ ./api/
COPY website/ ./website/
RUN pip install --no-cache-dir build && python -m build --wheel

FROM python:3.12-slim
WORKDIR /app
RUN adduser --disabled-password --gecos '' appuser
COPY --from=builder /app/dist/*.whl /tmp/
RUN pip install --no-cache-dir /tmp/*.whl && rm /tmp/*.whl
RUN mkdir -p data && chown appuser:appuser data
USER appuser
EXPOSE 4000
VOLUME ["/app/data"]
CMD ["llm-pico", "--host", "0.0.0.0", "--port", "4000", "--db", "/app/data/llm-pico.db"]
