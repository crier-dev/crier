FROM python:3.12-alpine
WORKDIR /srv
RUN pip install --no-cache-dir requests
COPY echo_sink.py .
EXPOSE 9002
CMD ["python", "echo_sink.py"]
