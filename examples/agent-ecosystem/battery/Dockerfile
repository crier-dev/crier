FROM python:3.12-alpine
WORKDIR /battery
COPY battery.sh .
RUN apk add --no-cache curl bash
ENTRYPOINT ["bash", "battery.sh"]
