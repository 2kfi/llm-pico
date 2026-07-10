FROM python:3.12-slim AS builder
WORKDIR /app
COPY pyproject.toml .
COPY llm_pico/ ./llm_pico/
RUN pip install --no-cache-dir build && python -m build --wheel

FROM python:3.12-slim
WORKDIR /app
RUN adduser --disabled-password --gecos '' appuser
COPY --from=builder /app/dist/*.whl /tmp/
RUN pip install --no-cache-dir /tmp/*.whl && rm /tmp/*.whl
COPY config.example.yaml ./config.yaml
COPY users.example.yaml ./users.yaml
RUN mkdir -p data && chown appuser:appuser data config.yaml users.yaml
USER appuser
EXPOSE 4000
VOLUME ["/app/data"]
ENV LLM_PICO_DB_PATH=/app/data/llm-pico.db
CMD ["llm-pico", "--host", "0.0.0.0", "--port", "4000", "--config", "config.yaml", "--users", "users.yaml", "--db", "/app/data/llm-pico.db"]
