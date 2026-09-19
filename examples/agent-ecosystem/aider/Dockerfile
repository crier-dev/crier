FROM python:3.12-alpine
WORKDIR /srv
# The shared consumer is a node script; aider itself is python.
RUN apk add --no-cache nodejs git
# aider pulls heavy deps — a failed install must still boot canned mode.
RUN pip install --no-cache-dir aider-chat 2>/dev/null || echo 'aider install failed — canned only'
# aider needs a git repo for the live path (--no-auto-commits keeps it read-only)
RUN git init -q .
COPY consumer/consumer.mjs registration-payloads.json ./
EXPOSE 9105
ENV AGENT_ID=aider HARNESS=aider AIDER_LIVE=0
CMD ["node", "consumer.mjs"]
