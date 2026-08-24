FROM node:22-alpine
WORKDIR /srv
RUN npm install -g opencode-ai@latest --no-audit --no-fund 2>/dev/null || true
COPY consumer.mjs .
EXPOSE 9102
CMD ["node", "consumer.mjs"]
