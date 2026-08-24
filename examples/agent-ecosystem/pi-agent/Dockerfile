FROM node:22-alpine
WORKDIR /srv
COPY pi-agent/package.json .
RUN npm install --no-audit --no-fund 2>/dev/null || true
COPY consumer/consumer.mjs .
EXPOSE 9101
ENV AGENT_ID=pi-agent HARNESS=pi-agent
CMD ["node", "consumer.mjs"]
