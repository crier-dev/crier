FROM node:22-alpine
WORKDIR /srv
RUN npm install -g opencode-ai@latest --no-audit --no-fund 2>/dev/null || true
COPY consumer/consumer.mjs registration-payloads.json ./
EXPOSE 9102
ENV AGENT_ID=opencode HARNESS=opencode OPENCODE_LIVE=0
CMD ["node", "consumer.mjs"]
