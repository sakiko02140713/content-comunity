# AI 服务镜像：零第三方依赖，仅需 Python 标准库
FROM python:3.11-slim

WORKDIR /app

# 服务只使用标准库（http.server / urllib / multiprocessing / concurrent.futures），
# 因此无需 pip install，镜像构建快且体积小。
COPY *.py ./

ENV PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    AI_HOST=0.0.0.0 \
    AI_PORT=8000

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8000/health',timeout=3).status==200 else 1)"

CMD ["python", "main.py"]
