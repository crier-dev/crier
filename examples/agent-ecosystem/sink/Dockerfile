FROM python:3.12-alpine
WORKDIR /srv
RUN pip install --no-cache-dir requests
COPY sink/echo_sink.py registration-payloads.json ./
EXPOSE 9002
CMD ["python", "echo_sink.py"]
