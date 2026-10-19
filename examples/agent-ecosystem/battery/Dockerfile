FROM python:3.12-alpine
WORKDIR /battery
COPY battery.sh check-harness.py ./
RUN apk add --no-cache curl bash
ENTRYPOINT ["bash", "battery.sh"]
