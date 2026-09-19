FROM alpine:3.20
WORKDIR /srv
# goose is a rust binary — node base is not right; the shared consumer needs node.
RUN apk add --no-cache nodejs curl
# Download the musl goose binary. Releases ship a .tar.gz (a bare
# goose-x86_64-unknown-linux-musl asset 404s — verified v1.47.0); extract the
# binary, then failure-tolerant to canned-only mode.
RUN curl -fsSL https://github.com/block/goose/releases/latest/download/goose-x86_64-unknown-linux-musl.tar.gz -o /tmp/goose.tgz \
    && tar -xzf /tmp/goose.tgz -C /usr/local/bin ./goose \
    && chmod +x /usr/local/bin/goose \
    && rm /tmp/goose.tgz \
    || echo 'goose download failed — canned mode only'
COPY consumer/consumer.mjs registration-payloads.json ./
EXPOSE 9106
ENV AGENT_ID=goose HARNESS=goose GOOSE_LIVE=0
CMD ["node", "consumer.mjs"]
